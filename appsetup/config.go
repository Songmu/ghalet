package appsetup

import (
	"errors"
	"os"
	"path/filepath"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

type preparedConfig struct {
	options          Options
	before, proposed []byte
	target           settings.Target
	managedPath      string
	managedBefore    []byte
	managed          *settings.Managed
	targetExists     bool
}

func setupConfig(options Options) (preparedConfig, error) {
	prepared, err := selectConfig(options)
	if err != nil {
		return preparedConfig{}, err
	}
	config, err := settings.ParseConfig(prepared.options.Config, prepared.proposed)
	if err != nil {
		return preparedConfig{}, err
	}
	prepared.managedPath = config.ManagedFile
	prepared.managed, prepared.managedBefore, err = settings.ReadManaged(config.ManagedFile)
	return prepared, err
}

func selectConfig(options Options) (preparedConfig, error) {
	if options.Config == "" && options.Systemd {
		options.Config = "/etc/ghalet/config.yaml"
	}
	path, err := settings.ResolvePath(options.Config)
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
	config, err := settings.ParseConfig(path, data)
	if err != nil {
		return preparedConfig{}, err
	}
	key, err := settings.TargetKey(options.Scope, options.RunnerGroup, options.ScaleSet)
	if err != nil {
		return preparedConfig{}, err
	}
	options.credentialName = key
	for _, target := range config.Targets {
		if target.ID == key {
			return preparedConfig{options: options, before: before, proposed: data, target: target, targetExists: true}, nil
		}
	}
	data, err = appendTarget(data, options.Scope, options.RunnerGroup, options.ScaleSet)
	if err != nil {
		return preparedConfig{}, err
	}
	config, err = settings.ParseConfig(path, data)
	if err != nil {
		return preparedConfig{}, err
	}
	return preparedConfig{options: options, before: before, proposed: data, target: config.Targets[len(config.Targets)-1]}, nil
}

func appendTarget(data []byte, scope, group, scaleSet string) ([]byte, error) {
	editor, err := settings.NewEditor(data, nil)
	if err != nil {
		return nil, err
	}
	if err := editor.AppendTarget(scope, group, scaleSet); err != nil {
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
	return os.ReadFile(path)
}

func targetConfig(data []byte, target string, state savedApp, installationID int64) ([]byte, error) {
	editor, err := settings.NewEditor(data, nil)
	if err != nil {
		return nil, err
	}
	if err := editor.SetTargetApp(target, state.ClientID, state.KeyPath, installationID); err != nil {
		return nil, err
	}
	return editor.Marshal()
}
