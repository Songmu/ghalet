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
	configeditor "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

type preparedConfig struct {
	options          Options
	before, proposed []byte
	target           ghalet.Target
}

func setupConfig(options Options) (preparedConfig, error) {
	if options.Config == "" {
		if options.Systemd {
			options.Config = "/etc/ghalet/config.yaml"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return preparedConfig{}, err
			}
			options.Config = filepath.Join(home, ".config", "ghalet", "config.yaml")
		}
	}
	path, err := filepath.Abs(options.Config)
	if err != nil {
		return preparedConfig{}, err
	}
	options.Config = path
	before, err := configSnapshot(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return preparedConfig{}, err
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
		return preparedConfig{}, err
	}
	var matches []ghalet.Target
	for _, target := range config.Targets {
		if options.ID != "" && target.ID == options.ID {
			if !strings.EqualFold(target.URL, "https://github.com/"+options.Scope) {
				return preparedConfig{}, errors.New("--id already belongs to a target with a different scope")
			}
			return preparedConfig{options: options, before: before, proposed: data, target: target}, nil
		}
		if strings.EqualFold(target.URL, "https://github.com/"+options.Scope) {
			matches = append(matches, target)
		}
	}
	if options.ID == "" {
		if len(matches) > 1 {
			return preparedConfig{}, errors.New("multiple targets match --scope; select one with --id")
		}
		if len(matches) == 1 {
			options.ID = matches[0].ID
			return preparedConfig{options: options, before: before, proposed: data, target: matches[0]}, nil
		}
		options.ID = strings.NewReplacer("/", "-", ".", "-").Replace(strings.ToLower(options.Scope))
	}
	if err := validateID(options.ID); err != nil {
		return preparedConfig{}, fmt.Errorf("%w; specify --id explicitly", err)
	}
	for _, target := range config.Targets {
		if target.ID == options.ID {
			return preparedConfig{}, errors.New("derived target ID collides with another scope; specify --id explicitly")
		}
	}
	data, err = appendTarget(data, options.ID, options.Scope)
	if err != nil {
		return preparedConfig{}, err
	}
	config, err = ghalet.ParseConfig(path, data)
	if err != nil {
		return preparedConfig{}, err
	}
	return preparedConfig{options: options, before: before, proposed: data, target: config.Targets[len(config.Targets)-1]}, nil
}

func appendTarget(data []byte, id, scope string) ([]byte, error) {
	editor, err := configeditor.NewEditor(data, nil)
	if err != nil {
		return nil, err
	}
	if err := editor.AppendTarget(id, scope); err != nil {
		return nil, err
	}
	return editor.Marshal()
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
	editor, err := configeditor.NewEditor(data, nil)
	if err != nil {
		return nil, err
	}
	if err := editor.SetTargetApp(target, state.ClientID, state.KeyPath, installationID); err != nil {
		return nil, err
	}
	return editor.Marshal()
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
