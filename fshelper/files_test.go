package fshelper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureAndCheckDirectory(t *testing.T) {
	parent := testDirectory(t)
	dir := filepath.Join(parent, "first", "second")
	if err := CheckDirectory(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing directory was not reported:", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "first")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checking a directory created it:", err)
	}
	if err := EnsureDirectory(dir, 0711); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0711 {
			t.Fatalf("%s permissions: %o", path, info.Mode().Perm())
		}
	}
	if err := EnsureDirectory(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := CheckDirectory(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0711 {
		t.Fatal("existing directory permissions changed")
	}
}

func TestDirectoriesRejectUnsafePaths(t *testing.T) {
	parent := testDirectory(t)
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(testDirectory(t), link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"relative":         "relative",
		"root":             "/",
		"file":             file,
		"file ancestor":    filepath.Join(file, "child"),
		"symlink":          link,
		"symlink ancestor": filepath.Join(link, "child"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := EnsureDirectory(path, 0700); err == nil {
				t.Fatal("unsafe path accepted for creation")
			}
			if err := CheckDirectory(path); err == nil {
				t.Fatal("unsafe path accepted for checking")
			}
		})
	}
}

func TestWriteFileAtomic(t *testing.T) {
	parent := testDirectory(t)
	dest := filepath.Join(parent, "metadata.json")
	if err := os.WriteFile(dest, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(dest, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "new" {
		t.Fatalf("file contents %q, error %v", data, err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("file permissions are not private")
	}
	directory := filepath.Join(parent, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(directory, []byte("invalid"), 0600); err == nil {
		t.Fatal("replacing a directory succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "directory" && entry.Name() != "metadata.json" {
			t.Fatalf("temporary file was left behind: %s", entry.Name())
		}
	}
	if err := WriteFileAtomic(filepath.Join(parent, "missing", "file"), nil, 0600); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing parent directory was not reported:", err)
	}
}
