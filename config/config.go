// Package config loads and validates ghalet settings and storage policy.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/actions/scaleset"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

type Duration time.Duration

const DefaultRunnerVersion = "2.338.0"

var dayDuration = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)d$`)

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var value string
	if err := unmarshal(&value); err != nil {
		return err
	}
	if strings.HasSuffix(value, "d") {
		if !dayDuration.MatchString(value) {
			return errors.New("day durations must be a single number followed by d")
		}
		value = strings.TrimSuffix(value, "d") + "h"
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		if parsed > time.Duration(1<<63-1)/24 || parsed < -time.Duration(1<<63-1)/24 {
			return errors.New("duration in days overflows time.Duration")
		}
		*d = Duration(parsed * 24)
		return nil
	}
	parsed, err := time.ParseDuration(value)
	*d = Duration(parsed)
	return err
}

type Config struct {
	ManagedFile string         `yaml:"managedFile"`
	Defaults    DefaultsConfig `yaml:"defaults"`
	Capacity    CapacityConfig `yaml:"capacity"`
	Targets     []Target       `yaml:"targets"`
	Runner      RunnerConfig   `yaml:"runner"`
	Logs        LogsConfig     `yaml:"logs"`
	Path        string         `yaml:"-"`
	secretNames map[string]bool
}

type DefaultsConfig struct {
	Credential *Credential `yaml:"credential"`
}

type CapacityConfig struct {
	MaxRunners int `yaml:"maxRunners"`
}

type Credential struct {
	Type           string `yaml:"type"`
	Hostname       string `yaml:"hostname,omitempty"`
	TokenEnv       string `yaml:"tokenEnv,omitempty"`
	TokenFile      string `yaml:"tokenFile,omitempty"`
	ClientID       string `yaml:"clientId,omitempty"`
	PrivateKeyFile string `yaml:"privateKeyFile,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
}

func (c *Credential) UnmarshalYAML(unmarshal func(any) error) error {
	type plain Credential
	value := plain{Type: "gh"}
	if err := unmarshal(&value); err != nil {
		return err
	}
	*c = Credential(value)
	return nil
}

type Target struct {
	ID            string        `yaml:"-"` // Derived from the registration, never configured.
	URL           string        `yaml:"scope"`
	Credential    Credential    `yaml:"credential"`
	RunnerGroup   string        `yaml:"runnerGroup"`
	ScaleSetName  string        `yaml:"scaleSet"`
	Scaling       ScalingConfig `yaml:"scaling"`
	credentialSet bool
}

func (t *Target) UnmarshalYAML(unmarshal func(any) error) error {
	type plain Target
	value := plain{
		Credential: Credential{Type: "gh"},
		Scaling:    ScalingConfig{MaxRunners: 2},
	}
	if err := unmarshal(&value); err != nil {
		return err
	}
	var fields map[string]any
	if err := unmarshal(&fields); err != nil {
		return err
	}
	*t = Target(value)
	_, t.credentialSet = fields["credential"]
	return nil
}

type ScalingConfig struct {
	MinRunners int `yaml:"minRunners"`
	MaxRunners int `yaml:"maxRunners"`
}

type RunnerConfig struct {
	Version        string            `yaml:"version"`
	User           string            `yaml:"user"`
	StartupTimeout Duration          `yaml:"startupTimeout"`
	DrainTimeout   Duration          `yaml:"drainTimeout"`
	WorkRoot       string            `yaml:"-"`
	CacheRoot      string            `yaml:"-"`
	Env            map[string]string `yaml:"env"`
}

type LogsConfig struct {
	Preserve  bool     `yaml:"preserve"`
	Retention Duration `yaml:"retention"`
	Root      string   `yaml:"root"`
}

// ResolvePath returns the lexical absolute configuration path, preserving symlinks.
func ResolvePath(path string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".config", "ghalet", "config.yaml")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Do not resolve symlinks: replacing the selected config must not change
	// the running daemon's control address.
	return filepath.Clean(absolute), nil
}

func storageDirectory(serviceEnv, xdgEnv, fallback string) (string, error) {
	// systemd supplies application directories; expect one path per variable.
	// https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#RuntimeDirectory=
	if path := os.Getenv(serviceEnv); path != "" {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, ':') {
			return "", fmt.Errorf("%s must specify a single absolute directory", serviceEnv)
		}
		return fshelper.ResolveDirectory(path)
	}
	// XDG paths are user-specific bases; unset or relative values use HOME.
	// https://specifications.freedesktop.org/basedir/latest/
	base := os.Getenv(xdgEnv)
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, fallback)
	}
	base, err := fshelper.ResolveDirectory(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ghalet"), nil
}

// StateDirectory returns the persistent application directory used by this
// process, resolving symlinks in its trusted storage base.
func StateDirectory() (string, error) {
	return storageDirectory("STATE_DIRECTORY", "XDG_STATE_HOME", filepath.Join(".local", "state"))
}

func LoadConfig(path string) (*Config, error) {
	path, err := ResolvePath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	return ParseConfig(path, content)
}

// ParseConfig resolves and validates configuration bytes without writing a file.
func ParseConfig(path string, content []byte) (*Config, error) {
	return parseConfig(path, content, nil)
}

// ParseConfigWithManaged validates a proposed static and managed configuration.
func ParseConfigWithManaged(path string, content []byte, managed *Managed) (*Config, error) {
	return parseConfig(path, content, managed)
}

func parseConfig(path string, content []byte, managed *Managed) (*Config, error) {
	path, err := ResolvePath(path)
	if err != nil {
		return nil, err
	}
	file, err := parser.ParseBytes(content, 0)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("config must contain exactly one YAML document")
	}
	base := filepath.Dir(path)
	stateDir, err := StateDirectory()
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	cacheDir, err := storageDirectory("CACHE_DIRECTORY", "XDG_CACHE_HOME", ".cache")
	if err != nil {
		return nil, fmt.Errorf("resolve runner cache directory: %w", err)
	}
	c := &Config{
		Path: path, Capacity: CapacityConfig{MaxRunners: 2},
		Runner: RunnerConfig{
			Version:        DefaultRunnerVersion,
			StartupTimeout: Duration(2 * time.Minute), DrainTimeout: Duration(10 * time.Minute),
			WorkRoot:  filepath.Join(stateDir, "runs"),
			CacheRoot: filepath.Join(cacheDir, "runners"),
		},
		Logs: LogsConfig{
			Preserve: true, Retention: Duration(7 * 24 * time.Hour),
			Root: filepath.Join(stateDir, "logs"),
		},
	}
	if err := yaml.NodeToValue(file.Docs[0].Body, c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		return filepath.Clean(path)
	}
	if c.Logs.Root == "" {
		return nil, errors.New("directory paths cannot be empty")
	}
	c.Logs.Root = resolve(c.Logs.Root)
	var configured struct {
		Logs struct {
			Root *string `yaml:"root"`
		} `yaml:"logs"`
	}
	if err := yaml.NodeToValue(file.Docs[0].Body, &configured); err != nil {
		return nil, err
	}
	if configured.Logs.Root != nil {
		c.Logs.Root, err = fshelper.ResolveDirectory(c.Logs.Root)
		if err != nil {
			return nil, fmt.Errorf("resolve log directory: %w", err)
		}
	}
	c.ManagedFile, err = ManagedPath(path, c.ManagedFile)
	if err != nil {
		return nil, fmt.Errorf("resolve managed file: %w", err)
	}
	if c.Defaults.Credential != nil {
		resolveCredentialPaths(c.Defaults.Credential, base)
	}
	for i := range c.Targets {
		target := &c.Targets[i]
		resolveCredentialPaths(&target.Credential, base)
		if !strings.Contains(target.URL, ":") {
			target.URL = "https://github.com/" + target.URL
		}
		target.URL = strings.TrimRight(target.URL, "/")
		target.ID, err = TargetKey(target.URL, target.RunnerGroup, target.ScaleSetName)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", target.URL, err)
		}
	}
	if managed == nil {
		managed, _, err = ReadManaged(c.ManagedFile)
		if err != nil {
			return nil, fmt.Errorf("load managed file: %w", err)
		}
	}
	c.mergeManaged(managed)
	for i := range c.Targets {
		target := &c.Targets[i]
		if !target.credentialSet && c.Defaults.Credential != nil {
			target.Credential = *c.Defaults.Credential
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var runnerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidTargetID reports whether an ID is safe for a target's managed directory.
func ValidTargetID(id string) bool {
	return safeID.MatchString(id)
}

// RunnerGroupName resolves an omitted group to GitHub's default runner group.
func RunnerGroupName(name string) string {
	if name == "" {
		return scaleset.DefaultRunnerGroup
	}
	return name
}

// TargetKey returns a filesystem-safe identity for a scope/group/scale set registration.
func TargetKey(scope, group, scaleSet string) (string, error) {
	if !strings.Contains(scope, ":") {
		scope = "https://github.com/" + scope
	}
	scope = strings.TrimRight(scope, "/")
	u, err := ScopeURL(scope)
	if err != nil {
		return "", err
	}
	if len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 2 && group != "" {
		return "", errors.New("repository scope cannot specify runnerGroup")
	}
	if scaleSet == "" || strings.TrimSpace(scaleSet) != scaleSet ||
		strings.ContainsAny(scaleSet, "\x00\r\n") || strings.ContainsAny(group, "\x00\r\n") {
		return "", errors.New("scaleSet is required and scale set or runner group name must be valid")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(scope) + "\x00" + RunnerGroupName(group) + "\x00" + scaleSet))
	return fmt.Sprintf("%x", sum), nil
}

func (c *Config) Validate() error {
	if c.Defaults.Credential != nil {
		if err := c.Defaults.Credential.validate(); err != nil {
			return fmt.Errorf("defaults.credential: %w", err)
		}
	}
	if c.Capacity.MaxRunners < 1 || c.Capacity.MaxRunners > 10000 {
		return errors.New("capacity.maxRunners must be between 1 and 10000")
	}
	if !runnerVersion.MatchString(c.Runner.Version) {
		return errors.New("runner.version must be a pinned numeric version (for example 2.338.0)")
	}
	if c.Runner.StartupTimeout <= 0 || c.Runner.DrainTimeout <= 0 || c.Logs.Retention <= 0 {
		return errors.New("runner.startupTimeout, runner.drainTimeout and logs.retention must be positive")
	}
	if c.Targets == nil {
		return errors.New("targets must be specified; use targets: [] to remove all targets")
	}
	registrations := map[string]bool{}
	minimum := 0
	for _, target := range c.Targets {
		if err := target.validate(); err != nil {
			return fmt.Errorf("target %s: %w", target.ID, err)
		}
		minimum += target.Scaling.MinRunners
		key, err := TargetKey(target.URL, target.RunnerGroup, target.ScaleSetName)
		if err != nil {
			return fmt.Errorf("target %s: %w", target.URL, err)
		}
		if registrations[key] {
			return fmt.Errorf("target %s: duplicate scope/group/scale set registration", target.ID)
		}
		registrations[key] = true
	}
	if minimum > c.Capacity.MaxRunners {
		return errors.New("sum of minRunners exceeds capacity.maxRunners")
	}
	if err := c.ValidateEnvironment(); err != nil {
		return err
	}
	return c.validateDirectories()
}

func (t Target) validate() error {
	u, err := ScopeURL(t.URL)
	if err != nil {
		return err
	}
	cred := t.Credential
	if err := cred.validate(); err != nil {
		return err
	}
	if cred.Type == "gh" && cred.Hostname != "" && cred.Hostname != u.Host {
		return errors.New("credential hostname does not match scope hostname")
	}
	if _, err := TargetKey(t.URL, t.RunnerGroup, t.ScaleSetName); err != nil {
		return err
	}
	if t.Scaling.MaxRunners < 1 || t.Scaling.MaxRunners > 10000 ||
		t.Scaling.MinRunners < 0 || t.Scaling.MinRunners > t.Scaling.MaxRunners {
		return errors.New("require 0 <= minRunners <= maxRunners <= 10000")
	}
	return nil
}

func (c Credential) validate() error {
	switch c.Type {
	case "gh":
		if c.TokenEnv != "" || c.TokenFile != "" || c.ClientID != "" || c.PrivateKeyFile != "" {
			return errors.New("gh credential accepts only hostname")
		}
	case "pat":
		if (c.TokenEnv == "") == (c.TokenFile == "") || c.ClientID != "" || c.PrivateKeyFile != "" || c.Hostname != "" {
			return errors.New("pat credential requires exactly one of tokenEnv or tokenFile")
		}
		if c.TokenEnv != "" && !envName.MatchString(c.TokenEnv) {
			return errors.New("invalid tokenEnv")
		}
	case "github-app":
		if c.ClientID == "" || c.PrivateKeyFile == "" || c.TokenEnv != "" || c.TokenFile != "" || c.Hostname != "" {
			return errors.New("github-app credential requires clientId and privateKeyFile")
		}
	default:
		return fmt.Errorf("unsupported credential type %q", c.Type)
	}
	if (c.Type == "github-app" && c.InstallationID <= 0) || (c.Type != "github-app" && c.InstallationID != 0) {
		return errors.New("credential.installationId is required only for github-app")
	}
	return nil
}

// ValidateEnvironment checks runner overrides against identity and secret policy.
func (c *Config) ValidateEnvironment() error {
	for name, value := range c.Runner.Env {
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) || c.SecretEnv(name) ||
			name == "HOME" || name == "USER" || name == "LOGNAME" || strings.HasPrefix(strings.ToUpper(name), "ACTIONS_RUNNER_INPUT_") {
			return fmt.Errorf("runner.env cannot set invalid, identity or management credential variable %q", name)
		}
	}
	return nil
}

func (c *Config) validateDirectories() error {
	paths := []string{c.Runner.WorkRoot, c.Runner.CacheRoot, c.Logs.Root}
	home, _ := os.UserHomeDir()
	if home != "" {
		var err error
		home, err = fshelper.ResolveDirectory(home)
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
	}
	config, err := fshelper.ResolveParent(c.Path)
	if err != nil {
		return fmt.Errorf("resolve config directory: %w", err)
	}
	for i, path := range paths {
		if within(path, c.ManagedFile) || within(c.ManagedFile, path) ||
			within(path, c.ManagedFile+".lock") || within(c.ManagedFile+".lock", path) {
			return fmt.Errorf("managedFile and its lock must not overlap runner or log directories: %s", c.ManagedFile)
		}
		if !filepath.IsAbs(path) || path == "/" || within(path, config) || (home != "" && within(path, home)) {
			return fmt.Errorf("unsafe managed directory %q", path)
		}
		for _, other := range paths[:i] {
			if within(path, other) || within(other, path) {
				return fmt.Errorf("managed directories must not overlap: %s and %s", path, other)
			}
		}
	}
	return nil
}

// ScopeURL validates and parses a GitHub organization or repository URL.
func ScopeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("scope URL must be an HTTPS organization or repository URL")
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimRight(u.Path, "/"), "/"), "/")
	if len(parts) < 1 || len(parts) > 2 || strings.EqualFold(parts[0], "enterprises") {
		return nil, errors.New("only repository and organization scopes are supported")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\ \x00") {
			return nil, errors.New("invalid organization or repository path")
		}
	}
	return u, nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RetainSecretEnvironment preserves credential variable history across applies.
func (c *Config) RetainSecretEnvironment(previous *Config) {
	c.secretNames = make(map[string]bool)
	for name := range previous.secretNames {
		c.secretNames[name] = true
	}
	for _, target := range previous.Targets {
		if name := target.Credential.TokenEnv; name != "" {
			c.secretNames[name] = true
		}
	}
	if previous.Defaults.Credential != nil && previous.Defaults.Credential.TokenEnv != "" {
		c.secretNames[previous.Defaults.Credential.TokenEnv] = true
	}
}

// SecretEnv reports management secrets, including previously configured names.
func (c *Config) SecretEnv(name string) bool {
	if c.secretNames[name] {
		return true
	}
	switch strings.ToUpper(name) {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"ACTIONS_RUNNER_INPUT_JITCONFIG":
		return true
	}
	if c.Defaults.Credential != nil && name != "" && c.Defaults.Credential.TokenEnv == name {
		return true
	}
	for _, target := range c.Targets {
		if target.Credential.TokenEnv == name && name != "" {
			return true
		}
	}
	return false
}
