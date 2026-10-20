package fshelper

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLockFileProcess(t *testing.T) {
	path := os.Getenv("GHALET_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	lock, err := LockFile(path)
	if err == nil {
		lock.Close()
		t.Fatal("another process acquired a held lock")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
}

func TestLockFileSurvivesAtomicReplacement(t *testing.T) {
	path := filepath.Join(testDirectory(t), "config.yaml")
	lock, err := LockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, data := range []string{"before", "after"} {
		if err := WriteFileAtomic(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=^TestLockFileProcess$")
		command.Env = append(os.Environ(), "GHALET_TEST_LOCK_PATH="+path+".lock")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("interprocess lock failed: %s: %v", output, err)
		}
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := LockFile(path + ".lock")
	if err != nil {
		t.Fatal("closing the lock did not release it", err)
	}
	next.Close()
}

func TestLockFileRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			directory := testDirectory(t)
			path := filepath.Join(directory, "config.lock")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(directory, "other"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "permissions":
				err = os.WriteFile(path, nil, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := LockFile(path); err == nil {
				lock.Close()
				t.Fatal("unsafe lock file accepted")
			}
		})
	}
}
