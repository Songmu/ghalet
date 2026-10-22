package fshelper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile locks an owner-only sidecar file until the returned file is closed.
// Configuration writers use configPath + ".lock" and acquire any credential
// lock first, keeping both locks through replacement and rollback.
// Root may use service-owned locks; new locks inherit the directory's ownership.
func LockFile(path string) (*os.File, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := CheckDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, err
	}
	if created && os.Geteuid() == 0 {
		parent, err := os.Stat(filepath.Dir(path))
		if err != nil {
			file.Close()
			return nil, err
		}
		stat := parent.Sys().(*syscall.Stat_t)
		if err := file.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			file.Close()
			return nil, err
		}
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || (os.Geteuid() != 0 && int(stat.Uid) != os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, fmt.Errorf("lock file must be a regular owner-only file owned by the current user: %s", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another file update is in progress for %s: %w", path, err)
	}
	return file, nil
}
