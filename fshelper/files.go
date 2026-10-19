// Package fshelper provides directory validation and atomic file writes for
// ghalet's managed files.
package fshelper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureDirectory creates missing directories with mode and verifies that path
// is absolute, is not the filesystem root, and contains no symlinks or
// non-directories. Newly created directories are chmodded to mode regardless of
// umask; existing directory permissions are unchanged. Validation does not
// protect subsequent path-based operations against concurrent replacement.
func EnsureDirectory(path string, mode os.FileMode) error {
	return managedDirectory(path, mode, true)
}

// CheckDirectory applies the same path validation as EnsureDirectory without
// creating directories or changing permissions.
func CheckDirectory(path string) error {
	return managedDirectory(path, 0, false)
}

func managedDirectory(path string, mode os.FileMode, create bool) error {
	if !filepath.IsAbs(path) || path == "/" {
		return fmt.Errorf("unsafe directory %s", path)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := os.Chmod(current, mode); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed path must not contain symlinks or non-directories: %s", current)
		}
	}
	return nil
}

// WriteFileAtomic writes data to a temporary file in dest's directory, sets its
// permissions to mode, syncs and closes it, and renames it over dest. It does not
// create or validate the parent directory, or sync that directory after rename.
func WriteFileAtomic(dest string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}
