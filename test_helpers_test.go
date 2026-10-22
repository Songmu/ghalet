package ghalet

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
	"github.com/Songmu/ghalet/logreduct"
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

var buildTargetID, _ = settings.TargetKey("owner/repository", "", "build")
var secondTargetID, _ = settings.TargetKey("owner/repository", "", "second")

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

func testConfig(t *testing.T) *settings.Config {
	t.Helper()
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("CACHE_DIRECTORY", "")
	t.Setenv("XDG_STATE_HOME", testDirectory(t))
	t.Setenv("XDG_CACHE_HOME", testDirectory(t))
	c, err := settings.LoadConfig(writeConfig(t, readFixture(t, "config/base.yaml")+"logs:\n  root: logs\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func seedDistribution(t *testing.T, c *settings.Config, script string, links ...[2]string) {
	t.Helper()
	if err := fshelper.EnsureDirectory(c.Runner.CacheRoot, 0700); err != nil {
		t.Fatal(err)
	}
	goos, arch := runtime.GOOS, runtime.GOARCH
	if goos == "darwin" {
		goos = "osx"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	name := "actions-runner-" + goos + "-" + arch + "-" + c.Runner.Version + ".tar.gz"
	archive := filepath.Join(c.Runner.CacheRoot, name)
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	compressed := gzip.NewWriter(io.MultiWriter(file, hash))
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{
		Name: "bin/Runner.Listener", Typeflag: tar.TypeReg, Mode: 0700, Size: int64(len(script)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, script); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if err := writer.WriteHeader(&tar.Header{
			Name: link[0], Typeflag: tar.TypeSymlink, Linkname: link[1],
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if err := os.WriteFile(filepath.Join(c.Runner.CacheRoot, name+".sha256"), []byte(digest+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	deleteErr        error
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
	if f.deleteErr != nil {
		return f.deleteErr
	}
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{ReplaceAttr: logreduct.Default.ReplaceAttr})))
	t.Cleanup(func() { slog.SetDefault(original) })
	d, err := newDaemon(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.cancel()
		d.workers.Wait()
	})
	if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0711); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	d.targets[buildTargetID] = &targetRuntime{Config: c.Targets[0], Client: api, Set: api.set}
	d.order = []string{buildTargetID}
	return d, api
}

func serveTestControl(t *testing.T, d *daemon) *controlServer {
	t.Helper()
	server, err := listenControl(d.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.close(); os.Remove(server.path + ".lock") })
	go server.serve(d)
	return server
}

func testState(t *testing.T, c *settings.Config) *RunState {
	t.Helper()
	account, err := resolveUser(c.Runner.User)
	if err != nil {
		t.Skip(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	dir := c.Runner.WorkRoot + "/" + buildTargetID + "/" + id
	return &RunState{Schema: 1, RunID: id, TargetID: buildTargetID,
		ScopeURL: "https://github.com/owner/repository", ScaleSetName: "build", ScaleSetID: 10,
		GroupID: 1, RunnerName: "runner-" + id, User: account.Name, UID: account.UID, Home: account.Home,
		RunnerDir: dir + "/runner", Workspace: dir + "/work", Phase: "preparing"}
}

var errTestAPI = errors.New("simulated API outage")
