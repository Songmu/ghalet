package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

const SystemConfigPath = "/etc/ghalet/config.yaml"

// Managed contains persistent setup information, not runnable target definitions
// or secret values. Targets become active only through the static configuration.
type Managed struct {
	Version  int             `yaml:"version"`
	Defaults DefaultsConfig  `yaml:"defaults,omitempty"`
	Targets  []ManagedTarget `yaml:"targets,omitempty"`
}

type ManagedTarget struct {
	ID             string     `yaml:"id"`
	Scope          string     `yaml:"scope"`
	Credential     Credential `yaml:"credential"`
	InstallationID int64      `yaml:"installationId,omitempty"`
}

// ManagedPath resolves an explicit path relative to the static configuration.
// Nonstandard configurations get isolated files in the process's state directory.
func ManagedPath(configPath, selected string) (string, error) {
	configPath, err := ResolvePath(configPath)
	if err != nil {
		return "", err
	}
	if selected != "" {
		if !filepath.IsAbs(selected) {
			selected = filepath.Join(filepath.Dir(configPath), selected)
		}
	} else {
		var root string
		if configPath == SystemConfigPath && os.Getenv("STATE_DIRECTORY") == "" {
			root, err = fshelper.ResolveDirectory("/var/lib/ghalet")
		} else {
			root, err = StateDirectory()
		}
		if err != nil {
			return "", err
		}
		standard, _ := ResolvePath("")
		if configPath == standard || configPath == SystemConfigPath {
			selected = filepath.Join(root, "managed.yaml")
		} else {
			sum := sha256.Sum256([]byte(configPath))
			selected = filepath.Join(root, "managed", fmt.Sprintf("%x.yaml", sum))
		}
		if err := fshelper.CheckDirectory(filepath.Dir(selected)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	path, err := fshelper.ResolveParent(filepath.Clean(selected))
	if err != nil {
		return "", err
	}
	static, err := fshelper.ResolveParent(configPath)
	if err != nil {
		return "", err
	}
	if path == static || path == static+".lock" || path+".lock" == static {
		return "", errors.New("managedFile must be separate from the configuration and its lock")
	}
	return path, nil
}

// ReadManaged returns an empty document when the file does not exist.
func ReadManaged(path string) (*Managed, []byte, error) {
	empty := &Managed{Version: 1}
	if err := fshelper.CheckDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil, nil
		}
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
		return nil, nil, errors.New("managed file must be a regular file owned by the current user and not writable by others")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, nil, errors.New("cannot read managed file (limit 1 MiB)")
	}
	managed, err := ParseManaged(data)
	return managed, data, err
}

func ParseManaged(data []byte) (*Managed, error) {
	file, err := parser.ParseBytes(data, 0)
	if err != nil || len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("managed file must contain exactly one YAML mapping")
	}
	var managed Managed
	if err := yaml.NodeToValue(file.Docs[0].Body, &managed, yaml.Strict()); err != nil {
		return nil, errors.New("managed file contains invalid or unknown fields")
	}
	if managed.Version != 1 {
		return nil, errors.New("unsupported managed file version")
	}
	if managed.Defaults.Credential != nil {
		if err := managed.Defaults.Credential.validate(); err != nil {
			return nil, fmt.Errorf("managed defaults.credential: %w", err)
		}
	}
	ids := make(map[string]bool)
	for _, target := range managed.Targets {
		if !ValidTargetID(target.ID) || ids[target.ID] {
			return nil, errors.New("invalid or duplicate managed target ID")
		}
		ids[target.ID] = true
		if _, err := ScopeURL(target.Scope); err != nil {
			return nil, fmt.Errorf("managed target %s: %w", target.ID, err)
		}
		if err := target.Credential.validate(); err != nil {
			return nil, fmt.Errorf("managed target %s: %w", target.ID, err)
		}
		if (target.Credential.Type == "github-app" && target.InstallationID <= 0) ||
			(target.Credential.Type != "github-app" && target.InstallationID != 0) {
			return nil, errors.New("managed installationId is required only for github-app")
		}
	}
	return &managed, nil
}

func (managed *Managed) Marshal() ([]byte, error) {
	data, err := yaml.Marshal(managed)
	if err != nil {
		return nil, err
	}
	if _, err := ParseManaged(data); err != nil {
		return nil, err
	}
	return append([]byte("# Managed by ghalet; static configuration takes precedence.\n"), data...), nil
}

func (managed *Managed) SetTarget(target ManagedTarget) {
	for i := range managed.Targets {
		if managed.Targets[i].ID == target.ID {
			managed.Targets[i] = target
			return
		}
	}
	managed.Targets = append(managed.Targets, target)
}

func resolveCredentialPaths(credential *Credential, base string) {
	for _, path := range []*string{&credential.TokenFile, &credential.PrivateKeyFile} {
		if *path != "" && !filepath.IsAbs(*path) {
			*path = filepath.Join(base, *path)
		}
	}
}

func (c *Config) mergeManaged(managed *Managed) error {
	base := filepath.Dir(c.ManagedFile)
	if c.Defaults.Credential == nil && managed.Defaults.Credential != nil {
		credential := *managed.Defaults.Credential
		resolveCredentialPaths(&credential, base)
		c.Defaults.Credential = &credential
	}
	for i := range c.Targets {
		target := &c.Targets[i]
		if target.credentialSet || target.installationSet {
			continue
		}
		for _, stored := range managed.Targets {
			if stored.ID != target.ID {
				continue
			}
			if !strings.EqualFold(strings.TrimRight(stored.Scope, "/"), target.URL) {
				return fmt.Errorf("managed target %s has a different scope; rerun setup or remove its stale managed entry", target.ID)
			}
			target.Credential, target.InstallationID = stored.Credential, stored.InstallationID
			resolveCredentialPaths(&target.Credential, base)
			target.credentialSet = true
			break
		}
	}
	return nil
}

// WriteManaged commits a prepared snapshot without overwriting another writer.
func WriteManaged(path string, before, after []byte) error {
	if _, err := ParseManaged(after); err != nil {
		return err
	}
	if err := fshelper.EnsureDirectory(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := fshelper.LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	_, current, err := ReadManaged(path)
	if err != nil {
		return err
	}
	if (before == nil) != (current == nil) || !bytes.Equal(before, current) {
		return errors.New("managed file changed during setup; saved App is retained, rerun setup")
	}
	return fshelper.WriteFileAtomic(path, after, 0600)
}
