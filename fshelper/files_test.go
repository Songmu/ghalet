package fshelper

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveTrustedDirectory(t *testing.T) {
	base := testDirectory(t)
	link := filepath.Join(testDirectory(t), "administrator-layout")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "missing/nested"} {
		got, err := ResolveDirectory(filepath.Join(link, suffix))
		want := filepath.Join(base, suffix)
		if err != nil || got != want {
			t.Fatalf("resolved %q, want %q: %v", got, want, err)
		}
	}
	missing := filepath.Join(base, "missing")
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resolution created a directory", err)
	}
	resolved, err := ResolveDirectory(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testDirectory(t), filepath.Join(base, "child")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectory(filepath.Join(resolved, "child", "nested"), 0700); err == nil {
		t.Fatal("resolving a trusted base allowed a managed child symlink")
	}
	leaf, err := ResolveParent(filepath.Join(link, "child"))
	if err != nil || leaf != filepath.Join(base, "child") {
		t.Fatal("parent resolution followed the final component", leaf, err)
	}
	if err := CheckDirectory(leaf); err == nil {
		t.Fatal("resolved parent bypassed final-component validation")
	}
}

func TestResolveDirectoryRejectsInvalidBases(t *testing.T) {
	base := testDirectory(t)
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "absent"), link); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", file, filepath.Join(file, "child"), link, filepath.Join(link, "child"), loop} {
		if _, err := ResolveDirectory(path); err == nil {
			t.Fatalf("invalid trusted directory accepted: %s", path)
		}
	}
}

func TestResolveMacOSVar(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS system layout")
	}
	path, err := ResolveDirectory("/var")
	if err != nil || path != "/private/var" {
		t.Fatal("macOS /var was not canonicalized", path, err)
	}
	if err := CheckDirectory(path); err != nil {
		t.Fatal("canonical macOS base was rejected", err)
	}
}

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
		"cleaned root":     "/../",
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

func TestEnsureDirectoryRetriesFailedParentSync(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires permission checks as a non-root user")
	}
	parent := testDirectory(t)
	if err := os.Chmod(parent, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0700); err != nil {
			t.Error(err)
		}
	})
	dir := filepath.Join(parent, "first", "second")
	for range 2 {
		if err := EnsureDirectory(dir, 0700); !errors.Is(err, os.ErrPermission) {
			t.Fatal("directory creation skipped a failed parent sync", err)
		}
		if _, err := os.Lstat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed parent sync left an unsynced directory for a retry", err)
		}
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectory(dir, 0700); err != nil {
		t.Fatal("directory creation did not recover after parent sync was restored", err)
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
