package fshelper

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile locks an owner-only sidecar file until the returned file is closed.
// Configuration writers use configPath + ".lock" and acquire any credential
// lock first, keeping both locks through replacement and rollback.
func LockFile(path string) (*os.File, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := CheckDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, fmt.Errorf("lock file must be a regular owner-only file owned by the current user: %s", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another file update is in progress for %s: %w", path, err)
	}
	return file, nil
}
