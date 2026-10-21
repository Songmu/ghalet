package ghalet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
)

func TestRecoverySeparatesUnrelatedFilesFromUnknownExecutions(t *testing.T) {
	for _, kind := range []string{"regular files", "unrecorded directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			d, _ := testDaemon(t)
			d.config.Capacity.MaxRunners = 1
			parent := filepath.Join(d.config.Runner.WorkRoot, "build")
			if err := fshelper.EnsureDirectory(parent, 0755); err != nil {
				t.Fatal(err)
			}
			paths := []string{filepath.Join(parent, "unrelated")}
			wantOccupied := 1
			switch kind {
			case "regular files":
				wantOccupied = 0
				paths = append(paths, filepath.Join(d.config.Runner.WorkRoot, ".DS_Store"))
				for _, path := range paths {
					if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "unrecorded directory":
				if err := os.Mkdir(paths[0], 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(testDirectory(t), paths[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			if d.occupied() != wantOccupied || len(d.problems) != wantOccupied {
				t.Fatal("recovery confused unrelated files with unknown executions", d.occupied(), d.problems)
			}
			session := &fakeSession{}
			scaler := &targetScaler{daemon: d, target: d.targets["build"], session: session}
			if err := scaler.Scale(t.Context(), availableMessage(1, 1)); err != nil || len(session.acquired) != 1-wantOccupied {
				t.Fatal("unexpected acquisition capacity", session.acquired, err)
			}
			response := d.gc(t.Context(), false)
			if (response.Error != "") != (wantOccupied != 0) {
				t.Fatal("unexpected garbage collection diagnostics", response)
			}
			for _, path := range paths {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("recovery or garbage collection removed an unrelated entry", err)
				}
			}
		})
	}
}
