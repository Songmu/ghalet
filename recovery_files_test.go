package ghalet

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/procgroup"
	"github.com/actions/scaleset"
)

type recoveryLookupAPI struct {
	scaleAPI
	runner *scaleset.RunnerReference
	err    error
	calls  int
}

func (a *recoveryLookupAPI) GetRunner(context.Context, int) (*scaleset.RunnerReference, error) {
	a.calls++
	return a.runner, a.err
}

func TestRecoverRunMatchingAndDiagnosticPrecedence(t *testing.T) {
	for _, name := range []string{
		"matched", "offline matched", "missing runner", "lookup error", "runner ID", "runner name", "runner scale set",
		"missing target", "target fault", "registration and user", "user", "inspection and target",
	} {
		t.Run(name, func(t *testing.T) {
			d, api := testDaemon(t)
			state := testState(t, d.config)
			state.RunnerID, state.Phase = 101, "idle"
			state.Process = procgroup.ProcessIdentity{PID: 12345, PGID: 12345, UID: state.UID, Started: "recorded start"}
			table := map[int]procgroup.Entry{
				state.Process.PID: {ProcessIdentity: state.Process, Command: filepath.Join(state.RunnerDir, "bin", "Runner.Listener")},
			}
			lookup := &recoveryLookupAPI{
				scaleAPI: api,
				runner:   &scaleset.RunnerReference{ID: state.RunnerID, Name: state.RunnerName, RunnerScaleSetID: state.ScaleSetID},
			}
			target := d.targets[buildTargetID]
			target.Client = lookup
			want := "live process cannot be matched to its GitHub runner registration"
			wantCalls := 1
			switch name {
			case "matched":
				want = ""
			case "offline matched":
				d.offline, want = true, ""
			case "missing runner":
				lookup.runner = nil
			case "lookup error":
				lookup.err = errTestAPI
			case "runner ID":
				lookup.runner.ID++
			case "runner name":
				lookup.runner.Name += "-other"
			case "runner scale set":
				lookup.runner.RunnerScaleSetID++
			case "missing target":
				delete(d.targets, buildTargetID)
				want, wantCalls = "state does not correspond to a currently configured target", 0
			case "target fault":
				target.Fault = "authentication failed"
				want, wantCalls = "registration cannot be verified; resolve target authentication/registration and restart", 0
			case "registration and user":
				target.Config.ScaleSetName += "-other"
				state.Home += "-other"
				want, wantCalls = "recorded registration differs from the current explicit scope/group/scale set", 0
			case "user":
				state.Home += "-other"
				want, wantCalls = "recorded execution user differs from the current execution user", 0
			case "inspection and target":
				delete(d.targets, buildTargetID)
				entry := table[state.Process.PID]
				entry.Started += "-reused"
				table[state.Process.PID] = entry
				want, wantCalls = "PID was reused or identity changed; refusing to signal or clean", 0
			}
			d.recoverRun(t.Context(), state, table)
			run := d.runs[state.RunID]
			if run == nil || run.State != state || run.Uncertain != (want != "") || state.Error != want ||
				run.Exited || run.GroupStopped || state.ProcessDone || lookup.calls != wantCalls {
				t.Fatalf("recovery changed matching, diagnostic precedence or capacity: run=%+v, calls=%d, want=%q", run, lookup.calls, want)
			}
			if want == "" && !reflect.DeepEqual(state.Members, []procgroup.ProcessIdentity{state.Process}) {
				t.Fatal("matching recovery did not retain observed membership")
			}
			_, err := os.Stat(filepath.Join(filepath.Dir(state.RunnerDir), "state.json"))
			if want == "" && !d.offline {
				if err != nil {
					t.Fatal("verified recovery was not persisted", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("offline or uncertain recovery wrote state", err)
			}
		})
	}
}

func TestRecoverySeparatesUnrelatedFilesFromUnknownExecutions(t *testing.T) {
	for _, kind := range []string{"regular files", "unrecorded directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			d, _ := testDaemon(t)
			d.config.Capacity.MaxRunners = 1
			parent := filepath.Join(d.config.Runner.WorkRoot, buildTargetID)
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
			scaler := &targetScaler{daemon: d, target: d.targets[buildTargetID], session: session}
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
