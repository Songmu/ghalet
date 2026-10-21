package credentialstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

func snapshotReplacementCredential(path string, noReplace bool) (credentialFile, error) {
	file, err := snapshotCredentialFile(path)
	if err == nil && noReplace && file.exists {
		err = fmt.Errorf("credential already exists at %s; existing credential unchanged", path)
	}
	if err != nil {
		clear(file.data)
	}
	return file, err
}

func snapshotPreviousCredential(path, existingMessage string) (credentialFile, error) {
	file, err := snapshotCredentialFile(path)
	if err == nil && file.exists {
		err = fmt.Errorf(existingMessage, path)
	}
	if err != nil {
		clear(file.data)
	}
	return file, err
}

type credentialConfigUpdate struct {
	original   credentialFile
	data       []byte
	configPath string
	configData []byte
}

func prepareCredentialConfig(options Options, runtimePath, missingMessage string, reserved ...string) (credentialConfigUpdate, error) {
	if options.NoDefault {
		return credentialConfigUpdate{}, nil
	}
	static, err := os.ReadFile(options.Config)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentialConfigUpdate{}, errors.New(missingMessage)
		}
		return credentialConfigUpdate{}, err
	}
	var config settings.Config
	if _, err := settings.NewEditor(static, &config); err != nil {
		return credentialConfigUpdate{}, err
	}
	managedPath, err := settings.ManagedPath(options.Config, config.ManagedFile)
	if err != nil {
		return credentialConfigUpdate{}, err
	}
	if err := settings.CheckManagedPaths(managedPath, options.Config,
		append(reserved, runtimePath, runtimePath+".previous")...); err != nil {
		return credentialConfigUpdate{}, err
	}
	managed, before, err := settings.ReadManaged(managedPath)
	if err != nil {
		return credentialConfigUpdate{}, err
	}
	credential := &settings.Credential{Type: options.Type, TokenFile: runtimePath}
	if options.Type == "github-app" {
		clientID := options.ClientID
		if clientID == "" && config.Defaults.Credential != nil && config.Defaults.Credential.Type == "github-app" {
			clientID = config.Defaults.Credential.ClientID
		}
		if clientID == "" && managed.Defaults.Credential != nil && managed.Defaults.Credential.Type == "github-app" {
			clientID = managed.Defaults.Credential.ClientID
		}
		if clientID == "" {
			return credentialConfigUpdate{}, errors.New("GitHub App defaults require --client-id or an existing defaults.credential.clientId")
		}
		credential = &settings.Credential{Type: options.Type, ClientID: clientID, PrivateKeyFile: runtimePath}
	}
	managed.Defaults.Credential = credential
	data, err := managed.Marshal()
	return credentialConfigUpdate{
		original: credentialFile{path: managedPath, data: before, exists: before != nil, mode: 0600},
		data:     data, configPath: options.Config, configData: static,
	}, err
}

func (c credentialConfigUpdate) lock() (*os.File, error) {
	if err := fshelper.EnsureDirectory(filepath.Dir(c.original.path), 0700); err != nil {
		return nil, err
	}
	return fshelper.LockFile(c.original.path + ".lock")
}

func (c credentialConfigUpdate) unchanged() bool {
	static, err := os.ReadFile(c.configPath)
	if err != nil || !bytes.Equal(static, c.configData) {
		return false
	}
	_, current, err := settings.ReadManaged(c.original.path)
	return err == nil && (current == nil) == !c.original.exists && bytes.Equal(current, c.original.data)
}
