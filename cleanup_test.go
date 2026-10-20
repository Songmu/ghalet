package ghalet

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
)

func TestArchiveBeforeDeletionAndIdempotentRetry(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	fshelper.EnsureDirectory(filepath.Join(state.RunnerDir, "_diag", "nested"), 0700)
	os.WriteFile(filepath.Join(state.RunnerDir, "output.log"), []byte("management-secret\n"), 0600)
	os.WriteFile(filepath.Join(state.RunnerDir, "_diag", "Runner.log"), []byte("diagnostic\n"), 0600)
	os.WriteFile(filepath.Join(state.RunnerDir, "_diag", "nested", "Worker.log"), []byte("nested diagnostic\n"), 0600)
	os.WriteFile(filepath.Join(state.RunnerDir, ".credentials"), []byte("do-not-archive"), 0600)
	d.secrets.Add("management-secret")
	state.ProcessDone = true
	state.Phase = "cleaning"
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(d.config.Logs.Root, "runs", state.RunID)
	data, err := os.ReadFile(filepath.Join(archive, "output.log"))
	if err != nil || strings.Contains(string(data), "management-secret") {
		t.Fatal("logs were not archived with redaction", err)
	}
	data, err = os.ReadFile(filepath.Join(archive, "metadata.json"))
	var metadata archivedRun
	if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Scope != state.ScopeURL {
		t.Fatal("archive metadata did not identify the registration by URL", err)
	}
	if _, err := os.Stat(filepath.Join(archive, ".credentials")); !os.IsNotExist(err) {
		t.Fatal("runner credentials were archived")
	}
	if data, err := os.ReadFile(filepath.Join(archive, "_diag", "nested", "Worker.log")); err != nil || string(data) != "nested diagnostic\n" {
		t.Fatal("nested diagnostic log was not preserved", err)
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
	fshelper.EnsureDirectory(state.RunnerDir, 0700)
	source := filepath.Join(state.RunnerDir, "output.log")
	os.WriteFile(source, []byte("must-survive"), 0600)
	fshelper.EnsureDirectory(filepath.Dir(d.config.Logs.Root), 0700)
	os.WriteFile(d.config.Logs.Root, []byte("not a directory"), 0600)
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
	os.Remove(d.config.Logs.Root)
	response := d.gc(t.Context(), false)
	if response.Error != "" || len(d.runs) != 0 {
		t.Fatal("gc failed to retry the preserved archive", response.Error)
	}
}

func TestArchiveRetryMustEstablishDurabilityBeforeDeletingOriginals(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	state.ProcessDone, state.Phase = true, "cleaning"
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state.RunnerDir, "output.log")
	if err := os.WriteFile(source, []byte("must survive a failed archive retry"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := archiveLogs(d.config, state, d.secrets); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(d.config.Logs.Root, "runs", state.RunID)
	unsafe := filepath.Join(archive, "unsafe.log")
	if err := os.Symlink(source, unsafe); err != nil {
		t.Fatal(err)
	}
	run := &activeRun{State: state}
	d.runs[state.RunID] = run
	if err := d.cleanup(run); err == nil || !strings.Contains(err.Error(), "unsafe log archive entry") {
		t.Fatal("existing archive bypassed durability verification", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("failed archive retry removed original logs", err)
	}
	if err := os.Remove(unsafe); err != nil {
		t.Fatal(err)
	}
	if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] != nil {
		t.Fatal("archive cleanup did not resume after durability verification", response.Error)
	}
}

func TestLogRetentionOnlyExpiresCompletedArchives(t *testing.T) {
	c := testConfig(t)
	state := testState(t, c)
	stateStore{c.Runner.WorkRoot}.save(state)
	fshelper.EnsureDirectory(state.RunnerDir, 0700)
	source := filepath.Join(state.RunnerDir, "output.log")
	os.WriteFile(source, []byte("original"), 0600)
	if err := archiveLogs(c, state, &logreduct.Redactor{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.Logs.Root, "runs", state.RunID)
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

func TestLogRetentionRejectsSymlinkedDirectoryAncestry(t *testing.T) {
	for _, location := range []string{"runs", "logs.root", "ancestor"} {
		t.Run(location, func(t *testing.T) {
			c := testConfig(t)
			state := testState(t, c)
			if err := archiveLogs(c, state, &logreduct.Redactor{}); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(c.Logs.Root, "runs", state.RunID)
			data, err := json.Marshal(archivedRun{
				RunID: state.RunID, TargetID: state.TargetID, ArchivedAt: time.Now().Add(-30 * 24 * time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(archive, "metadata.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			view := testDirectory(t)
			link := filepath.Join(view, "linked")
			source := c.Logs.Root
			switch location {
			case "runs":
				source = filepath.Join(source, "runs")
				c.Logs.Root = view
				link = filepath.Join(view, "runs")
			case "logs.root":
				c.Logs.Root = link
			case "ancestor":
				source = filepath.Dir(source)
				c.Logs.Root = filepath.Join(link, "logs")
			}
			if err := os.Symlink(source, link); err != nil {
				t.Fatal(err)
			}
			for _, dryRun := range []bool{true, false} {
				if _, err := expireLogs(t.Context(), c, dryRun); err == nil {
					t.Fatal("retention accepted a symlinked managed directory")
				}
				if _, err := os.Stat(archive); err != nil {
					t.Fatal("retention removed an archive outside its safe directory chain", err)
				}
			}
		})
	}
}

func TestLogRetentionDoesNotCreateMissingDirectories(t *testing.T) {
	c := testConfig(t)
	parent := filepath.Join(testDirectory(t), "missing")
	c.Logs.Root = filepath.Join(parent, "logs")
	for _, dryRun := range []bool{true, false} {
		if results, err := expireLogs(t.Context(), c, dryRun); err != nil || len(results) != 0 {
			t.Fatal("missing retention directory was not a no-op", results, err)
		}
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention created missing directories", err)
	}
}

func TestLogLineLimitsApplyToLiveOutputAndArchives(t *testing.T) {
	longChunks := make([]string, 4096)
	for i := range longChunks {
		longChunks[i] = strings.Repeat("x", 1024)
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
		ready  int
	}{
		{
			name:   "exact limit",
			chunks: []string{strings.Repeat("x", maxLogLineBytes-len("secret-value")), "sec", "ret-value\n"},
			want:   strings.Repeat("x", maxLogLineBytes-len("secret-value")) + "***\n",
		},
		{
			name:   "single large write",
			chunks: []string{strings.Repeat("x", 4*1024*1024)},
			want:   omittedLogLine,
		},
		{
			name:   "incremental unterminated line",
			chunks: longChunks,
			want:   omittedLogLine,
		},
		{
			name:   "secret across omission boundary and resume",
			chunks: []string{strings.Repeat("x", maxLogLineBytes-3) + "sec", "ret-value", "\nsec", "ret-value\nListening for ", "Jobs\nsec", "ret-value"},
			want:   omittedLogLine + "***\nListening for Jobs\n***",
			ready:  1,
		},
		{
			name:   "oversized readiness line",
			chunks: []string{"Listening for Jobs", strings.Repeat("x", maxLogLineBytes), "\n"},
			want:   omittedLogLine,
		},
		{
			name:   "newline-inclusive secret",
			chunks: []string{"whole-line-", "secret\n", "next\n"},
			want:   "***next\n",
		},
		{
			name:   "multiline secret",
			chunks: []string{"-----BEGIN PRIVATE ", "KEY-----\nprivate-", "key-line-one\nprivate-key-line-", "two\n-----END PRIVATE KEY-----\nnext\n"},
			want:   "***\n***\n***\n***\nnext\n",
		},
		{
			name:   "overlapping secrets",
			chunks: []string{"shared-", "secret-private-", "suffix\nabc", "d\nabc", "d"},
			want:   "***\n***\n***",
		},
		{
			name:   "CRLF multiline secret with final partial line",
			chunks: []string{"-----BEGIN PRIVATE KEY-----\r", "\nprivate-key-line-", "one\r\nprivate-key-line-two\r\n-----END PRIVATE KEY-----"},
			want:   "***\r\n***\r\n***\r\n***",
		},
	} {
		for _, live := range []bool{true, false} {
			mode := "archive"
			if live {
				mode = "live"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				path := filepath.Join(testDirectory(t), "output.log")
				secret := &logreduct.Redactor{}
				secret.Add("secret-value")
				secret.Add("whole-line-secret\n")
				secret.Add("shared-secret")
				secret.Add("shared-secret-private-suffix")
				secret.Add("abc")
				secret.Add("bcd")
				secret.Add("-----BEGIN PRIVATE KEY-----\r\nprivate-key-line-one\r\nprivate-key-line-two\r\n-----END PRIVATE KEY-----")
				if live {
					file, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { file.Close() })
					ready := 0
					writer := &runnerOutput{file: file, secrets: secret, ready: func() { ready++ }}
					for _, chunk := range tc.chunks {
						if n, err := writer.Write([]byte(chunk)); err != nil || n != len(chunk) {
							t.Fatal("bounded output write failed", n, err)
						}
						if len(writer.buffer) > maxLogLineBytes {
							t.Fatal("unterminated output exceeded its buffer limit", len(writer.buffer))
						}
					}
					if err := writer.close(); err != nil {
						t.Fatal(err)
					}
					if ready != tc.ready {
						t.Fatal("readiness detection changed across chunks or accepted an omitted line", ready)
					}
				} else {
					readers := make([]io.Reader, len(tc.chunks))
					for i, chunk := range tc.chunks {
						readers[i] = strings.NewReader(chunk)
					}
					if err := copyRedacted(io.MultiReader(readers...), path, secret); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != tc.want {
					t.Fatalf("unexpected bounded/redacted output: got %d bytes, want %d (%v)", len(data), len(tc.want), err)
				}
			})
		}
	}
}

func TestRunnerOutputRedactsSplitSecretsAndDetectsReadiness(t *testing.T) {
	file, err := os.Create(filepath.Join(testDirectory(t), "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	secret := &logreduct.Redactor{}
	secret.Add("secret-value")
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
