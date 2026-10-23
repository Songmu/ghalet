package ghalet

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
	"github.com/Songmu/ghalet/runnerlog"
)

func TestCleanupArchivesBeforeDeletingExecution(t *testing.T) {
	d, _ := testDaemon(t)
	state := testState(t, d.config)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := fshelper.EnsureDirectory(state.RunnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.RunnerDir, "output.log"), []byte("management-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	var metadata runnerlog.Metadata
	if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.Scope != state.ScopeURL {
		t.Fatal("archive metadata did not identify the registration by URL", err)
	}
	if _, err := os.Stat(filepath.Dir(state.RunnerDir)); !os.IsNotExist(err) {
		t.Fatal("execution directory survived successful archive and cleanup")
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
	if d.occupied() != 0 || d.runs[state.RunID] == nil || !run.CleanupRetryAt.IsZero() {
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

func TestArchiveRetryPreservesOriginalsUntilArchiveIsValid(t *testing.T) {
	for _, failure := range []string{"symlink", "missing metadata", "invalid metadata", "wrong run", "wrong target"} {
		t.Run(failure, func(t *testing.T) {
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
			const content = "must survive a failed archive retry"
			if err := os.WriteFile(source, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runnerlog.Archive(d.config.Logs.Root, runnerlog.Run{RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeURL, RunnerDir: state.RunnerDir}, d.secrets); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(d.config.Logs.Root, "runs", state.RunID)
			metadataPath := filepath.Join(archive, "metadata.json")
			original, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			want := "existing log archive is incomplete or belongs to another execution"
			unsafe := filepath.Join(archive, "unsafe.log")
			switch failure {
			case "symlink":
				want = "unsafe log archive entry"
				err = os.Symlink(source, unsafe)
			case "missing metadata":
				err = os.Remove(metadataPath)
			case "invalid metadata":
				err = os.WriteFile(metadataPath, []byte("{"), 0600)
			default:
				var metadata runnerlog.Metadata
				if err := json.Unmarshal(original, &metadata); err != nil {
					t.Fatal(err)
				}
				if failure == "wrong run" {
					metadata.RunID += "-other"
				} else {
					metadata.TargetID += "-other"
				}
				data, marshalErr := json.Marshal(metadata)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(metadataPath, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(metadataPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			run := &activeRun{State: state}
			d.runs[state.RunID] = run
			if err := d.cleanup(run); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("existing archive bypassed validation", err)
			}
			if data, err := os.ReadFile(source); err != nil || string(data) != content || d.runs[state.RunID] != run {
				t.Fatal("failed archive retry removed original logs or execution state", err)
			}
			if data, err := os.ReadFile(metadataPath); !bytes.Equal(data, before) ||
				(failure == "missing metadata" && !errors.Is(err, os.ErrNotExist)) ||
				(failure != "missing metadata" && err != nil) {
				t.Fatal("failed archive retry replaced existing metadata", err)
			}
			if failure == "symlink" {
				if err := os.Remove(unsafe); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(metadataPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			if response := d.gc(t.Context(), false); response.Error != "" || d.runs[state.RunID] != nil {
				t.Fatal("archive cleanup did not resume after repairing the archive", response.Error)
			}
		})
	}
}

func TestLogRetentionThroughTrustedBaseAlias(t *testing.T) {
	c := testConfig(t)
	base := testDirectory(t)
	link := filepath.Join(testDirectory(t), "archive-layout")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	c, err := settings.ParseConfig(c.Path, []byte(readFixture(t, "config/base.yaml")+"logs:\n  root: "+link+"\n"))
	if err != nil || c.Logs.Root != base {
		t.Fatal("log base was not canonicalized", c, err)
	}
	state := testState(t, c)
	if err := runnerlog.Archive(c.Logs.Root, runnerlog.Run{RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeURL, RunnerDir: state.RunnerDir}, &logreduct.Redactor{}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "runs", state.RunID)
	metadata, err := json.Marshal(runnerlog.Metadata{
		RunID: state.RunID, TargetID: state.TargetID, ArchivedAt: time.Now().Add(-30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "metadata.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	outside := testDirectory(t)
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(archive, "outside")); err != nil {
		t.Fatal(err)
	}
	results, err := expireLogs(t.Context(), c, true)
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatal("retention dry-run could not inspect an owned archive", results, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("retention dry-run removed the archive", err)
	}
	results, err = expireLogs(t.Context(), c, false)
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatal("retention could not remove an owned archive", results, err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("owned archive was retained", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
		t.Fatal("retention followed a symlink outside the archive", err)
	}
}

func TestLogRetentionRejectsSymlinkedDirectoryAncestry(t *testing.T) {
	for _, location := range []string{"runs", "logs.root", "ancestor"} {
		t.Run(location, func(t *testing.T) {
			c := testConfig(t)
			state := testState(t, c)
			if err := runnerlog.Archive(c.Logs.Root, runnerlog.Run{RunID: state.RunID, TargetID: state.TargetID, Scope: state.ScopeURL, RunnerDir: state.RunnerDir}, &logreduct.Redactor{}); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(c.Logs.Root, "runs", state.RunID)
			data, err := json.Marshal(runnerlog.Metadata{
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
