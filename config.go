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
	APIVersion string         `yaml:"apiVersion"`
	Capacity   CapacityConfig `yaml:"capacity"`
	Targets    []Target       `yaml:"targets"`
	Runner     RunnerConfig   `yaml:"runner"`
	Cleanup    CleanupConfig  `yaml:"cleanup"`
	Path       string         `yaml:"-"`
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

type Target struct {
	ID             string        `yaml:"id"`
	URL            string        `yaml:"scope"`
	Credential     Credential    `yaml:"credential"`
	InstallationID int64         `yaml:"installationId"`
	RunnerGroup    string        `yaml:"runnerGroup"`
	ScaleSetName   string        `yaml:"scaleSet"`
	Scaling        ScalingConfig `yaml:"scaling"`
}

type ScalingConfig struct {
	MinRunners int `yaml:"minRunners"`
	MaxRunners int `yaml:"maxRunners"`
}

type RunnerConfig struct {
	Version          string            `yaml:"version"`
	User             string            `yaml:"user"`
	StartupTimeout   Duration          `yaml:"startupTimeout"`
	DrainTimeout     Duration          `yaml:"drainTimeout"`
	WorkRoot         string            `yaml:"workRoot"`
	DistributionRoot string            `yaml:"distributionRoot"`
	CacheRoot        string            `yaml:"cacheRoot"`
	Env              map[string]string `yaml:"env"`
}

type CleanupConfig struct {
	PreserveLogs bool     `yaml:"preserveLogs"`
	LogRetention Duration `yaml:"logRetention"`
	LogRoot      string   `yaml:"logRoot"`
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
	c := &Config{
		Path: path, Capacity: CapacityConfig{MaxRunners: 2},
		Runner: RunnerConfig{
			StartupTimeout: Duration(2 * time.Minute), DrainTimeout: Duration(10 * time.Minute),
			WorkRoot: "runs", DistributionRoot: "runners", CacheRoot: "downloads",
		},
		Cleanup: CleanupConfig{PreserveLogs: true, LogRetention: Duration(7 * 24 * time.Hour), LogRoot: "logs"},
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
	for _, path := range []*string{&c.Runner.WorkRoot, &c.Runner.DistributionRoot, &c.Runner.CacheRoot, &c.Cleanup.LogRoot} {
		if *path == "" {
			return nil, errors.New("directory paths cannot be empty")
		}
		*path = resolve(*path)
	}
	for i := range c.Targets {
		target := &c.Targets[i]
		if target.ScaleSetName == "" {
			target.ScaleSetName = target.ID
		}
		cred := &target.Credential
		if cred.TokenFile != "" {
			cred.TokenFile = resolve(cred.TokenFile)
		}
		if cred.PrivateKeyFile != "" {
			cred.PrivateKeyFile = resolve(cred.PrivateKeyFile)
		}
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
	if c.APIVersion != "ghalet/v1alpha1" {
		return errors.New("apiVersion must be ghalet/v1alpha1")
	}
	if c.Capacity.MaxRunners < 1 || c.Capacity.MaxRunners > 10000 {
		return errors.New("capacity.maxRunners must be between 1 and 10000")
	}
	if !runnerVersion.MatchString(c.Runner.Version) {
		return errors.New("runner.version must be a pinned numeric version (for example 2.338.0)")
	}
	if c.Runner.StartupTimeout <= 0 || c.Runner.DrainTimeout <= 0 || c.Cleanup.LogRetention <= 0 {
		return errors.New("startupTimeout, drainTimeout and logRetention must be positive")
	}
	if len(c.Targets) == 0 {
		return errors.New("at least one target is required")
	}
	ids, registrations := map[string]bool{}, map[string]bool{}
	minimum := 0
	for _, target := range c.Targets {
		if !safeID.MatchString(target.ID) || ids[target.ID] {
			return fmt.Errorf("invalid or duplicate target id %q", target.ID)
		}
		ids[target.ID] = true
		u, err := scopeURL(target.URL)
		if err != nil {
			return fmt.Errorf("target %s: %w", target.ID, err)
		}
		cred := target.Credential
		switch cred.Type {
		case "gh":
			if cred.TokenEnv != "" || cred.TokenFile != "" || cred.ClientID != "" || cred.PrivateKeyFile != "" {
				return fmt.Errorf("target %s: gh credential accepts only hostname", target.ID)
			}
		case "pat":
			if (cred.TokenEnv == "") == (cred.TokenFile == "") || cred.ClientID != "" || cred.PrivateKeyFile != "" || cred.Hostname != "" {
				return fmt.Errorf("target %s: pat credential requires exactly one of tokenEnv or tokenFile", target.ID)
			}
			if cred.TokenEnv != "" && !envName.MatchString(cred.TokenEnv) {
				return fmt.Errorf("target %s: invalid tokenEnv", target.ID)
			}
		case "github-app":
			if cred.ClientID == "" || cred.PrivateKeyFile == "" || cred.TokenEnv != "" || cred.TokenFile != "" || cred.Hostname != "" {
				return fmt.Errorf("target %s: github-app credential requires clientId and privateKeyFile", target.ID)
			}
		default:
			return fmt.Errorf("target %s: unsupported credential type %q", target.ID, cred.Type)
		}
		if (cred.Type == "github-app" && target.InstallationID <= 0) || (cred.Type != "github-app" && target.InstallationID != 0) {
			return fmt.Errorf("target %s: installationId is required only for github-app", target.ID)
		}
		if cred.Type == "gh" && cred.Hostname != "" && cred.Hostname != u.Host {
			return fmt.Errorf("target %s: credential hostname does not match scope hostname", target.ID)
		}
		if len(strings.Split(strings.Trim(u.Path, "/"), "/")) == 2 && target.RunnerGroup != "" {
			return fmt.Errorf("target %s: repository scope cannot specify runnerGroup", target.ID)
		}
		if target.ScaleSetName == "" || strings.TrimSpace(target.ScaleSetName) != target.ScaleSetName ||
			strings.ContainsAny(target.ScaleSetName, "\x00\r\n") || strings.ContainsAny(target.RunnerGroup, "\x00\r\n") {
			return fmt.Errorf("target %s: invalid scale set or runner group name", target.ID)
		}
		if target.Scaling.MaxRunners < 1 || target.Scaling.MaxRunners > 10000 ||
			target.Scaling.MinRunners < 0 || target.Scaling.MinRunners > target.Scaling.MaxRunners {
			return fmt.Errorf("target %s: require 0 <= minRunners <= maxRunners <= 10000", target.ID)
		}
		minimum += target.Scaling.MinRunners
		group := target.RunnerGroup
		if group == "" {
			group = "default"
		}
		key := strings.ToLower(target.URL) + "\x00" + group + "\x00" + target.ScaleSetName
		if registrations[key] {
			return fmt.Errorf("target %s: duplicate scope/group/scale set registration", target.ID)
		}
		registrations[key] = true
	}
	if minimum > c.Capacity.MaxRunners {
		return errors.New("sum of minRunners exceeds capacity.maxRunners")
	}
	for name, value := range c.Runner.Env {
		if !envName.MatchString(name) || strings.ContainsRune(value, 0) || c.secretEnv(name) ||
			name == "HOME" || name == "USER" || name == "LOGNAME" || strings.HasPrefix(strings.ToUpper(name), "ACTIONS_RUNNER_INPUT_") {
			return fmt.Errorf("runner.env cannot set invalid, identity or management credential variable %q", name)
		}
	}
	paths := []string{c.Runner.WorkRoot, c.Runner.DistributionRoot, c.Runner.CacheRoot, c.Cleanup.LogRoot}
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

func (c *Config) target(id string) (Target, error) {
	for _, target := range c.Targets {
		if target.ID == id {
			return target, nil
		}
	}
	return Target{}, fmt.Errorf("unknown target %q", id)
}

func scopeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("scope URL must be an HTTPS organization or repository URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
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
	switch strings.ToUpper(name) {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"ACTIONS_RUNNER_INPUT_JITCONFIG":
		return true
	}
	for _, target := range c.Targets {
		if target.Credential.TokenEnv == name && name != "" {
			return true
		}
	}
	return false
}
