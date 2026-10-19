package ghalet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	d, _ := testDaemon(t)
	d.config.Runner.Version = defaultRunnerVersion
	dest := filepath.Join(testDirectory(t), "runner")
	ctx, cancel := context.WithTimeout(t.Context(), preparationTimeout)
	defer cancel()
	if err := d.distribution.prepare(ctx, d.config, dest); err != nil {
		t.Fatal(err)
	}
	// A syntactically invalid config exercises the standard secret input and
	// argument masking without registering a runner or contacting GitHub.
	jit := base64.StdEncoding.EncodeToString([]byte("ghalet-invalid-jit-smoke-marker"))
	command := exec.CommandContext(ctx, filepath.Join(dest, "bin", "Runner.Listener"), "run")
	command.Dir = dest
	command.Env = runnerEnvironment(d.config, d.user, jit)
	output, err := command.CombinedOutput()
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
		t.Fatal("runner did not report processing the JIT input; check OS runtime prerequisites")
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
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 1 || c.Targets[0].Scaling.MinRunners != 0 {
		t.Fatal("live test requires exactly one dedicated repository target with minRunners=0")
	}
	target := c.Targets[0]
	u, err := scopeURL(target.URL)
	if err != nil || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 {
		t.Fatal("live test requires a repository scope")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	c.Runner.DrainTimeout = Duration(15 * time.Second)
	var logs lockedBuffer
	finished := make(chan error, 1)
	go func() { finished <- RunDaemon(ctx, c, &logs) }()
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
		entries, err := os.ReadDir(filepath.Join(c.Cleanup.LogRoot, "runs"))
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
