package ghalet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveBeforeDeletionAndIdempotentRetry(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	safeDirectory(filepath.Join(state.RunnerDir, "_diag"), 0700)
	os.WriteFile(filepath.Join(state.RunnerDir, "output.log"), []byte("management-secret\n"), 0600)
	os.WriteFile(filepath.Join(state.RunnerDir, "_diag", "Runner.log"), []byte("diagnostic\n"), 0600)
	os.WriteFile(filepath.Join(state.RunnerDir, ".credentials"), []byte("do-not-archive"), 0600)
	d.secrets.add("management-secret")
	state.ProcessDone = true
	state.Phase = "cleaning"
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(d.config.Cleanup.LogRoot, "runs", state.RunID)
	data, err := os.ReadFile(filepath.Join(archive, "output.log"))
	if err != nil || strings.Contains(string(data), "management-secret") {
		t.Fatal("logs were not archived with redaction", err)
	}
	if _, err := os.Stat(filepath.Join(archive, ".credentials")); !os.IsNotExist(err) {
		t.Fatal("runner credentials were archived")
	}
	if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !os.IsNotExist(err) {
		t.Fatal("execution directory survived successful archive and cleanup")
	}
	if err := archiveLogs(d.config, state, d.secrets); err != nil {
		t.Fatal("completed archive could not be reused on cleanup retry", err)
	}
}

func TestArchiveFailurePreservesOriginalAndReleasesProcessCapacity(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	d.store.save(state)
	safeDirectory(state.RunnerDir, 0700)
	source := filepath.Join(state.RunnerDir, "output.log")
	os.WriteFile(source, []byte("must-survive"), 0600)
	safeDirectory(filepath.Dir(d.config.Cleanup.LogRoot), 0700)
	os.WriteFile(d.config.Cleanup.LogRoot, []byte("not a directory"), 0600)
	state.ProcessDone, state.Phase = true, "cleaning"
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err == nil {
		t.Fatal("archive failure was ignored")
	}
	if d.occupied() != 0 || d.runs[state.RunID] == nil {
		t.Fatal("file-only cleanup should retain state, not a process capacity slot")
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "must-survive" {
		t.Fatal("original log was deleted on archive failure")
	}
	if len(d.statusLocked().Problems) == 0 {
		t.Fatal("incomplete cleanup is missing from diagnostics")
	}
	os.Remove(d.config.Cleanup.LogRoot)
	response := d.gc(t.Context(), false)
	if response.Error != "" || len(d.runs) != 0 {
		t.Fatal("gc failed to retry the preserved archive", response.Error)
	}
}

func TestLogRetentionOnlyExpiresCompletedArchives(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	stateStore{c.Runner.WorkRoot}.save(state)
	safeDirectory(state.RunnerDir, 0700)
	source := filepath.Join(state.RunnerDir, "output.log")
	os.WriteFile(source, []byte("original"), 0600)
	if err := archiveLogs(c, state, &secrets{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.Cleanup.LogRoot, "runs", state.RunID)
	metadata, _ := json.Marshal(archivedRun{RunID: state.RunID, TargetID: state.TargetID, ArchivedAt: time.Now().Add(-30 * 24 * time.Hour)})
	os.WriteFile(filepath.Join(dir, "metadata.json"), metadata, 0600)
	results, err := expireLogs(t.Context(), c, true)
	if err != nil || len(results) != 1 {
		t.Fatal(results, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dry-run removed logs")
	}
	if _, err := expireLogs(t.Context(), c, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("expired archive was not removed")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("retention deleted unarchived execution logs")
	}
}

func TestRunnerOutputRedactsSplitSecretsAndDetectsReadiness(t *testing.T) {
	file, err := os.Create(filepath.Join(testDirectory(t), "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	secret := &secrets{}
	secret.add("secret-value")
	ready := 0
	writer := &runnerOutput{file: file, secrets: secret, ready: func() { ready++ }}
	for _, value := range []string{"sec", "ret-value\nListening for ", "Jobs\nListening for Jobs\nsec", "ret-value"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file.Name())
	if ready != 1 || strings.Contains(string(data), "secret-value") || strings.Count(string(data), "***") != 2 {
		t.Fatalf("ready=%d output=%s", ready, data)
	}
}
