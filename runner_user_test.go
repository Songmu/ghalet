package ghalet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/procgroup"
)

func TestRunnerUserCommandKeepsSecretsOutOfArguments(t *testing.T) {
	request := runnerUserRequest{
		Operation: "launch", RunnerDir: "/runs/example/runner",
		Environment: []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=private-jit"},
	}
	command, err := runnerUserCommand(t.Context(), executionUser{UID: 123, GID: 456}, request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(command.Args, " "), "private-jit") ||
		strings.Contains(strings.Join(command.Env, " "), "private-jit") {
		t.Fatal("sudo exposed the JIT through arguments or environment")
	}
	var decoded runnerUserRequest
	if err := json.NewDecoder(command.Stdin).Decode(&decoded); err != nil ||
		len(decoded.Environment) != 1 || decoded.Environment[0] != request.Environment[0] {
		t.Fatal("runner setup was not sent through the private input pipe", err)
	}
	if command.Args[len(command.Args)-1] != filepath.Join(request.RunnerDir, "bin", "Runner.Listener") {
		t.Fatal("sudo launch omitted the command used for process identity verification")
	}
	if len(command.ExtraFiles) != 0 || strings.Contains(strings.Join(command.Args, " "), "-C") {
		t.Fatal("sudo requires descriptor-preservation privileges")
	}
}

func TestShareRunnerDirectoryKeepsFailedPreparationPrivate(t *testing.T) {
	directory, outside := testDirectory(t), testDirectory(t)
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(directory, "a")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(nested, "outside")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := shareRunnerDirectory(directory, os.Getegid()); err == nil {
		t.Fatal("shared a preparation tree containing a symlink")
	}
	for _, path := range []string{directory, secret} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("failed preparation exposed the tree or an outside file", path, info, err)
		}
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := shareRunnerDirectory(directory, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0770 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatal("completed preparation did not expose the shared tree", err)
	}
}

func TestReleaseRunnerDirectoryDoesNotFollowLinks(t *testing.T) {
	dir, outside := testDirectory(t), testDirectory(t)
	log := filepath.Join(dir, "private.log")
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(log, []byte("diagnostics"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("keep private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := releaseRunnerDirectory(dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{log, 0660}, {secret, 0600}} {
		info, err := os.Stat(tc.path)
		if err != nil || info.Mode().Perm() != tc.mode {
			t.Fatal("unexpected released file permissions", tc.path, info, err)
		}
	}
	if err := releaseRunnerDirectory(filepath.Join(dir, "outside")); err == nil {
		t.Fatal("released a symlinked execution directory")
	}
}

func TestRunnerUserRejectsInvalidOperations(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("helper refuses all operations as root")
	}
	for _, request := range []runnerUserRequest{
		{Operation: "unknown"},
		{Operation: "signal", Group: procgroup.Group{UID: os.Geteuid() + 1}},
		{Operation: "signal", Group: procgroup.Group{UID: os.Geteuid()}},
		{Operation: "release", RunnerDir: "/"},
	} {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := runRunnerUser(bytes.NewReader(append(data, '\n'))); err == nil {
			t.Fatal("accepted invalid runner operation", request)
		}
	}
}

func TestRunnerUserPreservesHelperFailure(t *testing.T) {
	dir := testDirectory(t)
	script := "#!/bin/sh\nprintf '%s\\n' '{\"error\":\"read workspace: input/output error\",\"errno\":5}'\nprintf 'runner diagnostic\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err := callRunnerUser(t.Context(), executionUser{}, runnerUserRequest{Operation: "release"})
	if !errors.Is(err, syscall.EIO) || !cleanupRetryable(err) ||
		!strings.Contains(err.Error(), "runner diagnostic") ||
		!strings.Contains(err.Error(), "read workspace") {
		t.Fatal("helper failure lost its cause, retryability or diagnostic", err)
	}
}

func TestSudoRunnerCleanupBeforeLaunch(t *testing.T) {
	d, _ := testDaemon(t)
	d.user.Sudo = true
	state := testState(t, d.config)
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := os.MkdirAll(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	t.Setenv("PATH", testDirectory(t))
	if err := d.cleanup(run); err != nil {
		t.Fatal("partial preparation required runner-user access", err)
	}
	if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial preparation files were retained", err)
	}
}

// The packaging workflow supplies isolated accounts and a command-scoped sudo
// policy for this test binary, then runs the test as the management account.
func TestSudoRunnerLifecycle(t *testing.T) {
	name := os.Getenv("GHALET_TEST_RUNNER_USER")
	if name == "" {
		t.Skip("requires a disposable host with a non-interactive runner-user sudo policy")
	}
	c := testConfig(t)
	c.Runner.User = name
	c.Runner.WorkRoot = filepath.Join(sharedExecutionTestDirectory(t), "runs")
	c.Logs.Preserve = true
	d, err := newDaemon(c)
	if err != nil {
		t.Fatal(err)
	}
	if !d.user.Sudo || d.user.UID == os.Geteuid() || os.Geteuid() == 0 {
		t.Fatal("test must use distinct, non-root management and runner accounts")
	}
	if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0755); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	d.targets[buildTargetID] = &targetRuntime{Config: c.Targets[0], Client: api, Set: api.set}
	d.order = []string{buildTargetID}
	t.Cleanup(func() {
		settleRuns(d)
		for _, run := range d.runs {
			if err := d.stop(run, true); err != nil {
				t.Error(err)
			}
		}
		d.cancel()
		d.workers.Wait()
	})
	secret := filepath.Join(testDirectory(t), "management-credential")
	if err := os.WriteFile(secret, []byte("management-private"), 0600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
test "$(id -u)" = %s
test ! -r ../state.json
test ! -w ..
test ! -r %q
umask 077
mkdir _diag ../work/private
printf 'test-jit-secret\n' > _diag/runner.log
printf 'workspace\n' > ../work/private/data
printf 'Listening for Jobs\n'
sleep 60
`, strconv.Itoa(d.user.UID), secret)
	seedDistribution(t, c, script)
	d.targets[buildTargetID].Assigned = 1
	run := waitForRun(t, d, func(run *activeRun) bool { return run.State.Ready })
	d.targets[buildTargetID].Assigned = 0
	if run.State.Process.UID != d.user.UID || run.State.Process.PGID != run.State.Process.PID {
		t.Fatal("sudo launch recorded the wrapper rather than the runner guard", run.State.Process)
	}
	if err := d.stop(run, true); err != nil {
		t.Fatal("non-root controller could not stop and clean the runner", err)
	}
	if _, err := os.Stat(filepath.Dir(run.State.RunnerDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runner-owned private files prevented cleanup", err)
	}
	data, err := os.ReadFile(filepath.Join(c.Logs.Root, "runs", run.State.RunID, "_diag", "runner.log"))
	if err != nil || bytes.Contains(data, []byte("test-jit-secret")) {
		t.Fatalf("private runner diagnostics were not archived and redacted: %q (%v)", data, err)
	}
}
