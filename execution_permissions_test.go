package ghalet

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
)

func assertPermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s: mode %04o, want %04o", path, got, want)
	}
}

func TestExecutionDirectoryMigration(t *testing.T) {
	c := testConfig(t)
	base := testDirectory(t)
	c.Runner.WorkRoot = filepath.Join(base, "runs")
	state := testState(t, c)
	dir := filepath.Dir(state.RunnerDir)
	if err := fshelper.EnsureDirectory(dir, 0711); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state.RunnerDir, state.Workspace, filepath.Join(base, "credentials")} {
		if err := fshelper.EnsureDirectory(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	credential := filepath.Join(base, "credentials", "key")
	if err := os.WriteFile(credential, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0711); err != nil {
		t.Fatal(err)
	}
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	assertPermissions(t, c.Runner.WorkRoot, 0755)
	store := stateStore{c.Runner.WorkRoot}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{c.Runner.WorkRoot, filepath.Dir(dir), dir} {
		assertPermissions(t, path, 0755)
	}
	for _, path := range []string{filepath.Join(dir, "state.json"), filepath.Join(c.Runner.WorkRoot, ".lock"), credential} {
		assertPermissions(t, path, 0600)
	}
	for _, path := range []string{state.RunnerDir, state.Workspace, filepath.Dir(credential)} {
		assertPermissions(t, path, 0700)
	}
	assertPermissions(t, base, 0711)
	// Cleanup must also accept an execution created by an older daemon.
	for _, path := range []string{c.Runner.WorkRoot, filepath.Dir(dir), dir} {
		if err := os.Chmod(path, 0711); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.removeFiles(state); err != nil {
		t.Fatal(err)
	}
	assertPermissions(t, c.Runner.WorkRoot, 0755)
	assertPermissions(t, filepath.Dir(dir), 0755)
	assertPermissions(t, credential, 0600)
}

func TestReserveExecutionDirectoryPermissions(t *testing.T) {
	d, _ := testDaemon(t)
	d.cancel()
	d.mu.Lock()
	defer d.mu.Unlock()
	for range 2 {
		if err := d.reserve(d.targets["build"]); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.runs) != 2 {
		t.Fatal("reservations did not create distinct run IDs")
	}
	for _, run := range d.runs {
		dir := filepath.Dir(run.State.RunnerDir)
		for _, path := range []string{d.config.Runner.WorkRoot, filepath.Dir(dir), dir} {
			assertPermissions(t, path, 0755)
		}
		assertPermissions(t, filepath.Join(dir, "state.json"), 0600)
	}
}

func TestExecutionDirectoryRejectsSymlinks(t *testing.T) {
	for _, level := range []string{"root", "target", "run"} {
		t.Run(level, func(t *testing.T) {
			c := testConfig(t)
			state := testState(t, c)
			path := c.Runner.WorkRoot
			switch level {
			case "target":
				path = filepath.Dir(filepath.Dir(state.RunnerDir))
			case "run":
				path = filepath.Dir(state.RunnerDir)
			}
			if err := fshelper.EnsureDirectory(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			external := testDirectory(t)
			if err := os.Chmod(external, 0711); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, path); err != nil {
				t.Fatal(err)
			}
			store := stateStore{c.Runner.WorkRoot}
			if err := store.save(state); err == nil {
				t.Fatal("save accepted a symlink")
			}
			if err := store.removeFiles(state); err == nil {
				t.Fatal("cleanup accepted a symlink")
			}
			if level == "root" {
				if lock, err := lockWorkRoot(path); err == nil {
					lock.close()
					t.Fatal("lock accepted a symlink")
				}
			}
			assertPermissions(t, external, 0711)
		})
	}
}

// Unlike t.TempDir, this has no owner-only ancestor blocking a different UID.
func sharedExecutionTestDirectory(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("", "ghalet-permissions-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecutionDirectoryAccessAsRunner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to check access as a different UID")
	}
	c := testConfig(t)
	c.Runner.User = "nobody"
	c.Runner.WorkRoot = filepath.Join(sharedExecutionTestDirectory(t), "runs")
	account, err := resolveUser(c.Runner.User)
	if err != nil {
		t.Fatal(err)
	}
	state := testState(t, c)
	dir := filepath.Dir(state.RunnerDir)
	if err := fshelper.EnsureDirectory(dir, 0711); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(state.RunnerDir, account.UID, account.GID); err != nil {
		t.Fatal(err)
	}
	probe := func(script string, paths ...string) error {
		t.Helper()
		command := exec.Command("/bin/sh", append([]string{"-ec", script, "access-check"}, paths...)...)
		command.SysProcAttr = processAttributes(account)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Logf("access check: %s", output)
		}
		return err
	}
	if err := probe(`ls "$1" >/dev/null`, dir); err == nil {
		t.Fatal("different UID could list the legacy root-owned run directory")
	}
	lock, err := lockWorkRoot(c.Runner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	for _, state := range []*RunState{state, testState(t, c)} {
		if err := (stateStore{c.Runner.WorkRoot}).save(state); err != nil {
			t.Fatal(err)
		}
		if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(state.RunnerDir, account.UID, account.GID); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(state.RunnerDir)
		if err := probe(`
dir=$1
while :; do
  ls "$dir" >/dev/null
  test -x "$dir"
  [ "$dir" != / ] || break
  dir=$(dirname "$dir")
done
for dir in "$2" "$3" "$4"; do
  test ! -w "$dir"
  test ! -e "$dir/unauthorized"
  if touch "$dir/unauthorized" 2>/dev/null; then exit 1; fi
done
test ! -r "$2/state.json"
test ! -r "$4/.lock"
`, state.RunnerDir, dir, filepath.Dir(dir), c.Runner.WorkRoot); err != nil {
			t.Fatal("runner could not list/traverse parents or could access management data", err)
		}
	}
}
