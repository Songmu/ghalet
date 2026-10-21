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
