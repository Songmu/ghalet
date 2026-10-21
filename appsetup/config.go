package appsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	var matches []settings.Target
	for _, target := range config.Targets {
		if options.ID != "" && target.ID == options.ID {
			if !strings.EqualFold(target.URL, "https://github.com/"+options.Scope) {
				return preparedConfig{}, errors.New("--id already belongs to a target with a different scope")
			}
			return preparedConfig{options: options, before: before, proposed: data, target: target, targetExists: true}, nil
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
			return preparedConfig{options: options, before: before, proposed: data, target: matches[0], targetExists: true}, nil
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
	config, err = settings.ParseConfig(path, data)
	if err != nil {
		return preparedConfig{}, err
	}
	return preparedConfig{options: options, before: before, proposed: data, target: config.Targets[len(config.Targets)-1]}, nil
}

func appendTarget(data []byte, id, scope string) ([]byte, error) {
	editor, err := settings.NewEditor(data, nil)
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
