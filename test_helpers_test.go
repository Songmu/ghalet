package ghalet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(testDirectory(t), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	c, err := LoadConfig(writeConfig(t, readFixture(t, "config/base.yaml")+"cleanup:\n  logRoot: logs\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeScaleAPI struct {
	mu               sync.Mutex
	group            *scaleset.RunnerGroup
	set              *scaleset.RunnerScaleSet
	runners          map[int]*scaleset.RunnerReference
	nextID           int
	getErr           error
	removeErr        error
	jitErr           error
	groupErr         error
	createCalls      int
	updateCalls      int
	removeCalls      int
	deleteCalls      int
	lastGroupName    string
	lastScaleSetName string
}

func newFakeAPI() *fakeScaleAPI {
	return &fakeScaleAPI{
		group: &scaleset.RunnerGroup{ID: 1, Name: "default"},
		set: &scaleset.RunnerScaleSet{ID: 10, Name: "build", RunnerGroupID: 1,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}, Statistics: &scaleset.RunnerScaleSetStatistic{}},
		runners: map[int]*scaleset.RunnerReference{}, nextID: 100,
	}
}

func (f *fakeScaleAPI) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastGroupName = name
	return f.group, f.groupErr
}

func (f *fakeScaleAPI) GetRunnerScaleSet(_ context.Context, _ int, name string) (*scaleset.RunnerScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastScaleSetName = name
	return f.set, f.getErr
}

func (f *fakeScaleAPI) GetRunnerScaleSetByID(context.Context, int) (*scaleset.RunnerScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set, f.getErr
}

func (f *fakeScaleAPI) CreateRunnerScaleSet(_ context.Context, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	copy := *set
	copy.ID = 10
	f.set = &copy
	return f.set, nil
}

func (f *fakeScaleAPI) UpdateRunnerScaleSet(_ context.Context, _ int, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	f.set = set
	return set, nil
}

func (f *fakeScaleAPI) DeleteRunnerScaleSet(context.Context, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.set = nil
	return nil
}

func (f *fakeScaleAPI) GenerateJitRunnerConfig(_ context.Context, settings *scaleset.RunnerScaleSetJitRunnerSetting, setID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	f.nextID++
	runner := &scaleset.RunnerReference{ID: f.nextID, Name: settings.Name, RunnerScaleSetID: setID}
	f.runners[runner.ID] = runner
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: runner, EncodedJITConfig: "test-jit-secret"}, nil
}

func (f *fakeScaleAPI) GetRunner(_ context.Context, id int) (*scaleset.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.runners[id], nil
}

func (f *fakeScaleAPI) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, runner := range f.runners {
		if runner.Name == name {
			return runner, nil
		}
	}
	return nil, nil
}

func (f *fakeScaleAPI) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.runners, int(id))
	return nil
}

func testDaemon(t *testing.T) (*daemon, *fakeScaleAPI) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("process lifecycle tests require a non-root execution user")
	}
	c := testConfig(t)
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{ReplaceAttr: RedactLogAttr})))
	t.Cleanup(func() { slog.SetDefault(original) })
	d, err := newDaemon(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.cancel()
		d.workers.Wait()
	})
	if err := safeDirectory(c.Runner.WorkRoot, 0711); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	d.targets["build"] = &targetRuntime{Config: c.Targets[0], Client: api, Set: api.set}
	d.order = []string{"build"}
	return d, api
}

func testState(t *testing.T, c *Config) *RunState {
	t.Helper()
	account, err := resolveUser(c.Runner.User)
	if err != nil {
		t.Skip(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dir := c.Runner.WorkRoot + "/build/" + id
	return &RunState{Schema: 1, RunID: id, TargetID: "build",
		ScopeURL: "https://github.com/owner/repository", ScaleSetName: "build", ScaleSetID: 10,
		GroupID: 1, RunnerName: "runner-" + id, User: account.Name, UID: account.UID, Home: account.Home,
		RunnerDir: dir + "/runner", Workspace: dir + "/work", Phase: "preparing"}
}

var errTestAPI = errors.New("simulated API outage")
