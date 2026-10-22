package ghalet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/logreduct"
	"github.com/Songmu/ghalet/runnerdist"
)

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *lockedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestOfficialRunnerJITInputSmoke(t *testing.T) {
	if os.Getenv("GHALET_TEST_OFFICIAL_RUNNER") != "1" {
		t.Skip("set GHALET_TEST_OFFICIAL_RUNNER=1 to download and exercise the pinned official runner")
	}
	c := testConfig(t)
	c.Runner.Version = settings.DefaultRunnerVersion
	if os.Geteuid() == 0 {
		c.Runner.User = "nobody"
		c.Runner.WorkRoot = filepath.Join(sharedExecutionTestDirectory(t), "runs")
	}
	d, err := newDaemon(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.cancel()
	// CI fetches the official digest with authentication outside the container.
	if digestFile := os.Getenv("GHALET_TEST_RUNNER_DIGEST_FILE"); digestFile != "" {
		digest, err := os.ReadFile(digestFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d.config.Runner.CacheRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.config.Runner.CacheRoot, filepath.Base(digestFile)), digest, 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := testState(t, c)
	if err := d.store.save(state); err != nil {
		t.Fatal(err)
	}
	dest := state.RunnerDir
	ctx, cancel := context.WithTimeout(t.Context(), preparationTimeout)
	defer cancel()
	if err := d.distribution.Prepare(ctx, runnerdist.Config{
		Version: d.config.Runner.Version, CacheRoot: d.config.Runner.CacheRoot,
	}, dest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state.Workspace, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{dest, state.Workspace} {
		if err := shareRunnerDirectory(dir, d.user.GID); err != nil {
			t.Fatal(err)
		}
	}
	links := []string{state.Workspace}
	if err := filepath.WalkDir(dest, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type()&os.ModeSymlink != 0 {
			links = append(links, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check := exec.CommandContext(ctx, "/bin/sh", append([]string{"-ec",
		`test -w "$1"; shift; for link do test -r "$link"; done`, "ghalet-link-access"}, links...)...)
	check.Dir, check.Env, check.SysProcAttr = dest, runnerEnvironment(d.config, d.user, ""), processAttributes(d.user)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("execution user cannot access the shared workspace or distribution links: %v\n%s", err, output)
	}
	// A syntactically invalid config exercises the standard secret input and
	// argument masking without registering a runner or contacting GitHub.
	jit := base64.StdEncoding.EncodeToString([]byte("ghalet-invalid-jit-smoke-marker"))
	run := func() ([]byte, error) {
		command := exec.CommandContext(ctx, filepath.Join(dest, "bin", "Runner.Listener"), "run")
		command.Dir = dest
		command.Env = runnerEnvironment(d.config, d.user, jit)
		command.SysProcAttr = processAttributes(d.user)
		return command.CombinedOutput()
	}
	if os.Geteuid() == 0 {
		if err := os.Chmod(filepath.Dir(dest), 0711); err != nil {
			t.Fatal(err)
		}
		output, err := run()
		if err == nil || !strings.Contains(string(output), "Permission to read the directory contents") {
			t.Fatalf("official runner did not reproduce the legacy parent permission error: %v\n%s", err, output)
		}
		if err := d.store.save(state); err != nil {
			t.Fatal(err)
		}
	}
	output, err := run()
	if err == nil {
		t.Fatal("invalid JIT configuration unexpectedly succeeded")
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	combined := string(output)
	filepath.WalkDir(filepath.Join(dest, "_diag"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				combined += string(data)
			}
		}
		return nil
	})
	if !strings.Contains(combined, "jitconfig") {
		t.Fatalf("runner did not report processing the JIT input; check OS runtime prerequisites:\n%s", combined)
	}
	if strings.Contains(combined, jit) {
		t.Fatal("official runner leaked encoded JIT input")
	}
}

func TestGitHubEphemeralLifecycle(t *testing.T) {
	path := os.Getenv("GHALET_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GHALET_INTEGRATION_CONFIG to a dedicated live acceptance configuration")
	}
	c, err := settings.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 1 || c.Targets[0].Scaling.MinRunners != 0 {
		t.Fatal("live test requires exactly one dedicated repository target with minRunners=0")
	}
	target := c.Targets[0]
	u, err := settings.ScopeURL(target.URL)
	if err != nil || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 {
		t.Fatal("live test requires a repository scope")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	c.Runner.DrainTimeout = settings.Duration(15 * time.Second)
	var logs lockedBuffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{ReplaceAttr: logreduct.Default.ReplaceAttr})))
	defer slog.SetDefault(original)
	finished := make(chan error, 1)
	go func() { finished <- RunDaemon(ctx, c) }()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("live daemon shutdown: %v", err)
			}
		case <-time.After(90 * time.Second):
			t.Error("live daemon did not drain; inspect the dedicated host")
		}
	}()
	waitLive(t, ctx, func() bool {
		response, err := controlCall(ctx, c.Path, controlRequest{Command: "status"})
		return err == nil && response.Status != nil && len(response.Status.Problems) == 0
	})
	repository := strings.Trim(u.Path, "/")
	gh := func(args ...string) []byte {
		t.Helper()
		callCtx, stop := context.WithTimeout(ctx, apiTimeout)
		defer stop()
		command := exec.CommandContext(callCtx, "gh", args...)
		command.Env = append(os.Environ(), "GH_HOST="+u.Host)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("live gh request failed: %v", err)
		}
		return output
	}
	seen := map[string]bool{}
	for range 2 {
		correlation, _ := newID()
		args := []string{"workflow", "run", "ghalet-integration.yaml", "--repo", repository,
			"--field", "scale_set=" + target.ScaleSetName, "--field", "correlation=" + correlation}
		if ref := os.Getenv("GHALET_INTEGRATION_REF"); ref != "" {
			args = append(args, "--ref", ref)
		}
		gh(args...)
		waitLive(t, ctx, func() bool {
			var result struct {
				Runs []struct {
					Title      string `json:"display_title"`
					Status     string `json:"status"`
					Conclusion string `json:"conclusion"`
				} `json:"workflow_runs"`
			}
			data := gh("api", fmt.Sprintf("repos/%s/actions/workflows/ghalet-integration.yaml/runs?event=workflow_dispatch&per_page=20", repository))
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			for _, run := range result.Runs {
				if run.Title == "ghalet integration "+correlation && run.Status == "completed" {
					if run.Conclusion != "success" {
						t.Fatalf("integration workflow concluded %s", run.Conclusion)
					}
					return true
				}
			}
			return false
		})
		waitLive(t, ctx, func() bool {
			response, err := controlCall(ctx, c.Path, controlRequest{Command: "status"})
			return err == nil && response.Status != nil && response.Status.Occupied == 0 && len(response.Status.Runs) == 0
		})
		entries, err := os.ReadDir(filepath.Join(c.Logs.Root, "runs"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if len(entry.Name()) == 32 {
				seen[entry.Name()] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected two distinct archived executions in an initially empty dedicated configuration, got %d", len(seen))
	}
	decoder := json.NewDecoder(strings.NewReader(logs.text()))
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal("daemon mixed non-JSON output into stdout", err)
		}
	}
}

func waitLive(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal("live acceptance timeout:", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
