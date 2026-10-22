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

// ResolveDirectory canonicalizes a trusted directory base without creating it.
// Existing symlinks are resolved; missing trailing directories are preserved.
// Use the result as a base for managed children, not to validate those children.
func ResolveDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("directory must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	var missing []string
	for {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, filepath.Base(path))
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return "", err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("not a directory: %s", path)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			path = filepath.Join(path, missing[i])
		}
		return path, nil
	}
}

// ResolveParent resolves a trusted parent directory while leaving the final
// component unchanged so callers can still reject a symlink at that location.
func ResolveParent(path string) (string, error) {
	parent, err := ResolveDirectory(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// EnsureDirectory creates missing directories with mode and verifies that path
// is absolute, is not the filesystem root, and contains no symlinks or
// non-directories. Newly created directories are chmodded to mode regardless of
// umask and their parents are synced before creating further children. Failed
// creation syncs remove the new empty directory so retries repeat the sync.
// Existing directory permissions are unchanged. Validation does not protect
// subsequent path-based operations against concurrent replacement.
// Resolve trusted bases before deriving managed paths, never managed children.
func EnsureDirectory(path string, mode os.FileMode) error {
	return managedDirectory(path, mode, true)
}

// CheckDirectory applies the same path validation as EnsureDirectory without
// creating directories or changing permissions.
func CheckDirectory(path string) error {
	return managedDirectory(path, 0, false)
}

// CheckSymlink requires a relative link whose lexical target and fully resolved
// target stay within root. Call after all targets exist, while the tree is private.
func CheckSymlink(root *os.Root, name string) error {
	target, err := root.Readlink(name)
	if err != nil {
		return err
	}
	if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Join(filepath.Dir(name), target)) {
		return fmt.Errorf("unsafe symlink %q -> %q", name, target)
	}
	if _, err := root.Stat(name); err != nil {
		return fmt.Errorf("unsafe symlink %q -> %q: %w", name, target, err)
	}
	return nil
}

func managedDirectory(path string, mode os.FileMode, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return fmt.Errorf("unsafe directory %s", path)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if create && errors.Is(err, os.ErrNotExist) {
			err = os.Mkdir(current, mode)
			if err == nil {
				err = os.Chmod(current, mode)
				if err == nil {
					err = SyncDirectory(filepath.Dir(current))
				}
				if err != nil {
					return errors.Join(err, os.Remove(current))
				}
			} else if !errors.Is(err, os.ErrExist) {
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

// SyncDirectory persists changes to the directory's entries.
func SyncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// WriteFileAtomic writes data to a temporary file in dest's directory, sets its
// permissions to mode, syncs and closes it, and renames it over dest. It does not
// create or validate the parent directory, or sync that directory after rename.
func WriteFileAtomic(dest string, data []byte, mode os.FileMode) error {
	return WriteFileAtomicWithOwner(dest, data, mode, -1, -1)
}

// WriteFileAtomicWithOwner sets the temporary file's owner before publishing it.
// A uid or gid of -1 leaves that ownership component unchanged.
func WriteFileAtomicWithOwner(dest string, data []byte, mode os.FileMode, uid, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if uid != -1 || gid != -1 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return err
		}
	}
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
