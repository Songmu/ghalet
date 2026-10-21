package appsetup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/goccy/go-yaml"
)

func setupConfig(options Options) (Options, []byte, []byte, ghalet.Target, error) {
	if options.Config == "" {
		if options.Systemd {
			options.Config = "/etc/ghalet/config.yaml"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return options, nil, nil, ghalet.Target{}, err
			}
			options.Config = filepath.Join(home, ".config", "ghalet", "config.yaml")
		}
	}
	path, err := filepath.Abs(options.Config)
	if err != nil {
		return options, nil, nil, ghalet.Target{}, err
	}
	options.Config = path
	before, err := configSnapshot(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return options, nil, nil, ghalet.Target{}, err
	}
	data := before
	if errors.Is(err, os.ErrNotExist) {
		before = nil
		data = []byte("targets: []\n")
		if options.Systemd {
			data = append(data, []byte("runner:\n  user: ghalet-runner\nlogs:\n  root: /var/log/ghalet/runners\n")...)
		}
	}
	config, err := ghalet.ParseConfig(path, data)
	if err != nil {
		return options, nil, nil, ghalet.Target{}, err
	}
	var matches []ghalet.Target
	for _, target := range config.Targets {
		if options.ID != "" && target.ID == options.ID {
			if !strings.EqualFold(target.URL, "https://github.com/"+options.Scope) {
				return options, nil, nil, ghalet.Target{}, errors.New("--id already belongs to a target with a different scope")
			}
			return options, before, data, target, nil
		}
		if strings.EqualFold(target.URL, "https://github.com/"+options.Scope) {
			matches = append(matches, target)
		}
	}
	if options.ID == "" {
		if len(matches) > 1 {
			return options, nil, nil, ghalet.Target{}, errors.New("multiple targets match --scope; select one with --id")
		}
		if len(matches) == 1 {
			options.ID = matches[0].ID
			return options, before, data, matches[0], nil
		}
		options.ID = strings.NewReplacer("/", "-", ".", "-").Replace(strings.ToLower(options.Scope))
	}
	if err := validateID(options.ID); err != nil {
		return options, nil, nil, ghalet.Target{}, fmt.Errorf("%w; specify --id explicitly", err)
	}
	for _, target := range config.Targets {
		if target.ID == options.ID {
			return options, nil, nil, ghalet.Target{}, errors.New("derived target ID collides with another scope; specify --id explicitly")
		}
	}
	data, err = appendTarget(data, options.ID, options.Scope)
	if err != nil {
		return options, nil, nil, ghalet.Target{}, err
	}
	config, err = ghalet.ParseConfig(path, data)
	if err != nil {
		return options, nil, nil, ghalet.Target{}, err
	}
	return options, before, data, config.Targets[len(config.Targets)-1], nil
}

func appendTarget(data []byte, id, scope string) ([]byte, error) {
	var document yaml.MapSlice
	comments := yaml.CommentMap{}
	if err := yaml.UnmarshalWithOptions(data, &document, yaml.UseOrderedMap(), yaml.CommentToMap(comments)); err != nil {
		return nil, err
	}
	for i := range document {
		if document[i].Key != "targets" {
			continue
		}
		targets, ok := document[i].Value.([]any)
		if !ok {
			return nil, errors.New("configuration targets must be a sequence")
		}
		document[i].Value = append(targets, yaml.MapSlice{
			{Key: "id", Value: id}, {Key: "scope", Value: scope},
			{Key: "credential", Value: yaml.MapSlice{}},
		})
		return yaml.MarshalWithOptions(document, yaml.WithComment(comments))
	}
	return nil, errors.New("configuration must contain targets")
}

func configSnapshot(path string) ([]byte, error) {
	path, err := fshelper.ResolveParent(path)
	if err != nil {
		return nil, err
	}
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
	path, err := fshelper.ResolveParent(path)
	if err != nil {
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
	current, err := configSnapshot(path)
	if err != nil && !(before == nil && errors.Is(err, os.ErrNotExist)) {
		return err
	}
	if (before == nil && err == nil) || !bytes.Equal(current, before) {
		return errors.New("configuration changed during setup; saved App is retained, rerun setup to review the current configuration")
	}
	return fshelper.WriteFileAtomic(path, after, 0600)
}
