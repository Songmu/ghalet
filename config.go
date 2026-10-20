package ghalet

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

type Duration time.Duration

const defaultRunnerVersion = "2.338.0"

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
	Hostname       string `yaml:"hostname"`
	TokenEnv       string `yaml:"tokenEnv"`
	TokenFile      string `yaml:"tokenFile"`
	ClientID       string `yaml:"clientId"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
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
	ID             string        `yaml:"id"`
	URL            string        `yaml:"scope"`
	Credential     Credential    `yaml:"credential"`
	InstallationID int64         `yaml:"installationId"`
	RunnerGroup    string        `yaml:"runnerGroup"`
	ScaleSetName   string        `yaml:"scaleSet"`
	Scaling        ScalingConfig `yaml:"scaling"`
	credentialSet  bool
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

func configPath(path string) (string, error) {
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
		return filepath.Clean(path), nil
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
	return filepath.Join(base, "ghalet"), nil
}

func LoadConfig(path string) (*Config, error) {
	path, err := configPath(path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	file, err := parser.ParseBytes(content, 0)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("config must contain exactly one YAML document")
	}
	base := filepath.Dir(path)
	stateDir, err := storageDirectory("STATE_DIRECTORY", "XDG_STATE_HOME", filepath.Join(".local", "state"))
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
			Version:        defaultRunnerVersion,
			StartupTimeout: Duration(2 * time.Minute), DrainTimeout: Duration(10 * time.Minute),
			WorkRoot:  filepath.Join(stateDir, "runs"),
			CacheRoot: filepath.Join(cacheDir, "runners"),
		},
		Logs: LogsConfig{
			Preserve: true, Retention: Duration(7 * 24 * time.Hour),
			Root: filepath.Join(stateDir, "logs"),
		},
	}
	if err := yaml.NodeToValue(file.Docs[0].Body, c, yaml.Strict()); err != nil {
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
	resolveCredential := func(cred *Credential) {
		if cred.TokenFile != "" {
			cred.TokenFile = resolve(cred.TokenFile)
		}
		if cred.PrivateKeyFile != "" {
			cred.PrivateKeyFile = resolve(cred.PrivateKeyFile)
		}
	}
	if c.Defaults.Credential != nil {
		resolveCredential(c.Defaults.Credential)
	}
	for i := range c.Targets {
		target := &c.Targets[i]
		if !target.credentialSet && c.Defaults.Credential != nil {
			target.Credential = *c.Defaults.Credential
		}
		if target.ScaleSetName == "" {
			target.ScaleSetName = target.ID
		}
		resolveCredential(&target.Credential)
		if !strings.Contains(target.URL, ":") {
			target.URL = "https://github.com/" + target.URL
		}
		target.URL = strings.TrimRight(target.URL, "/")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var runnerVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var envName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

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
	ids, registrations := map[string]bool{}, map[string]bool{}
	minimum := 0
	for _, target := range c.Targets {
		if !safeID.MatchString(target.ID) || ids[target.ID] {
			return fmt.Errorf("invalid or duplicate target id %q", target.ID)
		}
		ids[target.ID] = true
		if err := target.validate(); err != nil {
			return fmt.Errorf("target %s: %w", target.ID, err)
		}
		minimum += target.Scaling.MinRunners
		key := strings.ToLower(target.URL) + "\x00" + runnerGroupName(target.RunnerGroup) + "\x00" + target.ScaleSetName
		if registrations[key] {
			return fmt.Errorf("target %s: duplicate scope/group/scale set registration", target.ID)
		}
		registrations[key] = true
	}
	if minimum > c.Capacity.MaxRunners {
		return errors.New("sum of minRunners exceeds capacity.maxRunners")
	}
	if err := c.validateEnvironment(); err != nil {
		return err
	}
	return c.validateDirectories()
}

func (t Target) validate() error {
	u, err := scopeURL(t.URL)
	if err != nil {
		return err
	}
	cred := t.Credential
	if err := cred.validate(); err != nil {
		return err
	}
	if (cred.Type == "github-app" && t.InstallationID <= 0) || (cred.Type != "github-app" && t.InstallationID != 0) {
		return errors.New("installationId is required only for github-app")
	}
	if cred.Type == "gh" && cred.Hostname != "" && cred.Hostname != u.Host {
		return errors.New("credential hostname does not match scope hostname")
	}
	if len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 2 && t.RunnerGroup != "" {
		return errors.New("repository scope cannot specify runnerGroup")
	}
	if t.ScaleSetName == "" || strings.TrimSpace(t.ScaleSetName) != t.ScaleSetName ||
		strings.ContainsAny(t.ScaleSetName, "\x00\r\n") || strings.ContainsAny(t.RunnerGroup, "\x00\r\n") {
		return errors.New("invalid scale set or runner group name")
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
	return nil
}

func (c *Config) validateEnvironment() error {
	for name, value := range c.Runner.Env {
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) || c.secretEnv(name) ||
			name == "HOME" || name == "USER" || name == "LOGNAME" || strings.HasPrefix(strings.ToUpper(name), "ACTIONS_RUNNER_INPUT_") {
			return fmt.Errorf("runner.env cannot set invalid, identity or management credential variable %q", name)
		}
	}
	return nil
}

func (c *Config) validateDirectories() error {
	paths := []string{c.Runner.WorkRoot, c.Runner.CacheRoot, c.Logs.Root}
	home, _ := os.UserHomeDir()
	for i, path := range paths {
		if !filepath.IsAbs(path) || path == "/" || within(path, c.Path) || (home != "" && within(path, home)) {
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

func scopeURL(raw string) (*url.URL, error) {
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

func (c *Config) secretEnv(name string) bool {
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
