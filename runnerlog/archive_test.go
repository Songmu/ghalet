package runnerlog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Songmu/ghalet/logreduct"
)

func TestArchivePublishesAllowedLogsAndReusesCompletedArchive(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := Run{RunID: strings.Repeat("a", 32), TargetID: "target", Scope: "https://github.com/owner/repo", RunnerDir: filepath.Join(base, "runner")}
	for name, content := range map[string]string{
		"output.log":              "management-secret\n",
		"_diag/Runner.log":        "diagnostic\n",
		"_diag/nested/Worker.log": "nested diagnostic\n",
		".credentials":            "do-not-archive",
	} {
		path := filepath.Join(run.RunnerDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	secret := &logreduct.Redactor{}
	secret.Add("management-secret")
	logRoot := filepath.Join(base, "logs")
	if err := Archive(logRoot, run, secret); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(logRoot, "runs", run.RunID)
	for name, want := range map[string]string{
		"output.log":              "***\n",
		"_diag/Runner.log":        "diagnostic\n",
		"_diag/nested/Worker.log": "nested diagnostic\n",
	} {
		data, err := os.ReadFile(filepath.Join(archive, name))
		if err != nil || string(data) != want {
			t.Fatalf("archived %s = %q, %v; want %q", name, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(archive, ".credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runner credentials were archived", err)
	}
	data, err := os.ReadFile(filepath.Join(archive, "metadata.json"))
	var metadata Metadata
	if err != nil || json.Unmarshal(data, &metadata) != nil || metadata.RunID != run.RunID ||
		metadata.TargetID != run.TargetID || metadata.Scope != run.Scope || metadata.ArchivedAt.IsZero() {
		t.Fatal("archive metadata did not identify the execution", metadata, err)
	}
	output := filepath.Join(archive, "output.log")
	if err := os.Chmod(output, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(output, 0600); err != nil {
			t.Error(err)
		}
	})
	if err := os.RemoveAll(run.RunnerDir); err != nil {
		t.Fatal(err)
	}
	if err := Archive(logRoot, run, secret); err != nil {
		t.Fatal("completed archive retry reopened logs or required the original runner", err)
	}
}

func TestExpireOnlyRemovesCompletedExpiredArchives(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "runner", "output.log")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	logRoot := filepath.Join(base, "logs")
	expiredID, recentID, invalidID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	for id, age := range map[string]time.Duration{expiredID: 48 * time.Hour, recentID: time.Hour, invalidID: 0} {
		directory := filepath.Join(logRoot, "runs", id)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		metadata := Metadata{RunID: id, ArchivedAt: time.Now().Add(-age)}
		if id == invalidID {
			metadata.ArchivedAt = time.Time{}
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "metadata.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dryRun := range []bool{true, false} {
		results, err := Expire(t.Context(), logRoot, 24*time.Hour, dryRun)
		if err != nil || len(results) != 2 || results[0].Action != "expire archived logs" || results[1].Error == "" {
			t.Fatal("expiry lost the expired or invalid archive result", results, err)
		}
		for _, id := range []string{expiredID, recentID, invalidID} {
			_, err := os.Stat(filepath.Join(logRoot, "runs", id))
			if id == expiredID && !dryRun {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("expired archive was retained", err)
				}
			} else if err != nil {
				t.Fatal("dry-run, recent or invalid archive was removed", id, err)
			}
		}
		if data, err := os.ReadFile(source); err != nil || string(data) != "original" {
			t.Fatal("retention changed unarchived execution logs", err)
		}
	}
}

func TestArchiveRejectsSymlinkedSources(t *testing.T) {
	for _, source := range []string{".", "_diag", "output.log", "_diag/Runner.log"} {
		t.Run(source, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			runner := filepath.Join(base, "runner")
			if err := os.MkdirAll(filepath.Join(runner, "_diag"), 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(base, "original")
			if err := os.WriteFile(outside, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(runner, source)
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			logRoot := filepath.Join(base, "logs")
			run := Run{RunID: strings.Repeat("a", 32), RunnerDir: runner}
			if err := Archive(logRoot, run, &logreduct.Redactor{}); err == nil {
				t.Fatal("archive accepted a symlinked log source")
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "retained" {
				t.Fatal("archive failure modified the original", err)
			}
			entries, err := os.ReadDir(filepath.Join(logRoot, "runs"))
			if err != nil || len(entries) != 0 {
				t.Fatal("failed archive left published or staged logs", entries, err)
			}
		})
	}
}

type cancelAfterFirstEntry struct {
	context.Context
	calls int
}

func (c *cancelAfterFirstEntry) Err() error {
	c.calls++
	if c.calls > 1 {
		return context.Canceled
	}
	return nil
}

func TestExpireReturnsPartialResults(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		dir := filepath.Join(base, "runs", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(Metadata{RunID: id, ArchivedAt: time.Now().Add(-48 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &cancelAfterFirstEntry{Context: context.Background()}
	results, err := Expire(ctx, base, 24*time.Hour, true)
	if !errors.Is(err, context.Canceled) || len(results) != 1 || results[0].Action != "expire archived logs" {
		t.Fatal("partial expiry results lost on cancellation", results, err)
	}
}
