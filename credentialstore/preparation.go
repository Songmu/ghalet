package credentialstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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
	original credentialFile
	data     []byte
}

func prepareCredentialConfig(options Options, runtimePath, missingMessage string) (credentialConfigUpdate, error) {
	if options.NoDefault {
		return credentialConfigUpdate{}, nil
	}
	config, err := snapshotConfigFile(options.Config)
	if err != nil {
		return credentialConfigUpdate{}, err
	}
	if !config.exists {
		return credentialConfigUpdate{}, errors.New(missingMessage)
	}
	data, err := credentialConfig(config.data, options, runtimePath)
	return credentialConfigUpdate{original: config, data: data}, err
}

func (c credentialConfigUpdate) lock() (*os.File, error) {
	return fshelper.LockFile(c.original.path + ".lock")
}

func (c credentialConfigUpdate) unchanged() bool {
	current, err := os.ReadFile(c.original.path)
	return err == nil && bytes.Equal(current, c.original.data)
}
