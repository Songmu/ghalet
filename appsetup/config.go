package appsetup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/goccy/go-yaml"
)

func configSnapshot(path string) ([]byte, error) {
	if err := fshelper.CheckDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("configuration must be a regular file owned by the current user and not writable by others")
	}
	return os.ReadFile(path)
}

func targetConfig(data []byte, target string, state savedApp, installationID int64) ([]byte, error) {
	var identities struct {
		Targets []struct {
			ID string `yaml:"id"`
		} `yaml:"targets"`
	}
	if err := yaml.Unmarshal(data, &identities); err != nil {
		return nil, errors.New("cannot decode target IDs for configuration update")
	}
	var document yaml.MapSlice
	comments := yaml.CommentMap{}
	if err := yaml.UnmarshalWithOptions(data, &document, yaml.UseOrderedMap(), yaml.CommentToMap(comments)); err != nil {
		return nil, errors.New("cannot parse configuration for update")
	}
	found := false
	for _, item := range document {
		if item.Key != "targets" {
			continue
		}
		targets, ok := item.Value.([]any)
		if !ok {
			return nil, errors.New("configuration targets must be a sequence")
		}
		if len(targets) != len(identities.Targets) {
			return nil, errors.New("configuration target mappings do not match decoded target IDs")
		}
		for i, value := range targets {
			mapping, ok := value.(yaml.MapSlice)
			if !ok {
				return nil, errors.New("configuration target must be a mapping")
			}
			if identities.Targets[i].ID != target {
				continue
			}
			credential := yaml.MapSlice{
				{Key: "type", Value: "github-app"},
				{Key: "clientId", Value: state.ClientID},
				{Key: "privateKeyFile", Value: state.KeyPath},
			}
			setMapping(&mapping, "credential", credential)
			setMapping(&mapping, "installationId", installationID)
			targets[i] = mapping
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("target %q disappeared from the configuration", target)
	}
	return yaml.MarshalWithOptions(document, yaml.WithComment(comments))
}

func setMapping(mapping *yaml.MapSlice, key string, value any) {
	for i := range *mapping {
		if (*mapping)[i].Key == key {
			(*mapping)[i].Value = value
			return
		}
	}
	*mapping = append(*mapping, yaml.MapItem{Key: key, Value: value})
}

func writeConfig(path string, before, after []byte) error {
	lock, err := fshelper.LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := configSnapshot(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, before) {
		return errors.New("configuration changed during setup; saved App is retained, rerun setup to review the current configuration")
	}
	return fshelper.WriteFileAtomic(path, after, 0600)
}
