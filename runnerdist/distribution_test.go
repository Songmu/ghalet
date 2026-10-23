package runnerdist

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Songmu/ghalet/fshelper"
)

type archiveEntry struct {
	name string
	body string
	link string
	kind byte
	mode int64
}

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := testDirectory(t)
	return Config{
		Version:   "2.338.0",
		CacheRoot: filepath.Join(root, "runners"),
	}
}

func writeArchive(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		if entry.kind == 0 {
			entry.kind = tar.TypeReg
		}
		if entry.mode == 0 {
			entry.mode = 0700
		}
		size := int64(len(entry.body))
		if entry.kind != tar.TypeReg {
			size = 0
		}
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: entry.mode, Size: size, Linkname: entry.link}); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
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
}

func seedDistribution(t *testing.T, c Config, script string) {
	t.Helper()
	if err := fshelper.EnsureDirectory(c.CacheRoot, 0700); err != nil {
		t.Fatal(err)
	}
	platform, err := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	name := "actions-runner-" + platform + "-" + c.Version + ".tar.gz"
	archive := filepath.Join(c.CacheRoot, name)
	writeArchive(t, archive, []archiveEntry{{name: "bin/Runner.Listener", body: script}})
	digest, err := fileDigest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.CacheRoot, name+".sha256"), []byte(digest+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedDistributionAndIndependentExecutions(t *testing.T) {
	c := testConfig(t)
	seedDistribution(t, c, "#!/bin/sh\nexit 0\n")
	distribution := New(10 * time.Minute)
	for _, run := range []string{"first", "second"} {
		dest := filepath.Join(testDirectory(t), run)
		if err := distribution.Prepare(t.Context(), c, dest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "bin/Runner.Listener"), []byte("job modification"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	platform, _ := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	archive := filepath.Join(c.CacheRoot, "actions-runner-"+platform+"-"+c.Version+".tar.gz")
	os.WriteFile(archive, []byte("corrupt"), 0600)
	if err := distribution.Prepare(t.Context(), c, filepath.Join(testDirectory(t), "third")); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal("corrupt cache was accepted or silently replaced:", err)
	}
}

func TestNewPreservesHTTPTimeout(t *testing.T) {
	timeout := 10 * time.Minute
	if got := New(timeout).client.Timeout; got != timeout {
		t.Fatalf("HTTP timeout %s, want %s", got, timeout)
	}
}

func TestDownloadPublishesOnlyCompleteResponses(t *testing.T) {
	for _, name := range []string{"success", "HTTP error", "truncated", "canceled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch name {
				case "HTTP error":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				case "truncated":
					w.Header().Set("Content-Length", "100")
				}
				fmt.Fprint(w, "runner archive")
				if name == "canceled" {
					w.(http.Flusher).Flush()
					cancel()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			directory := testDirectory(t)
			dest := filepath.Join(directory, "runner.tar.gz")
			if err := os.WriteFile(dest, []byte("original cache"), 0600); err != nil {
				t.Fatal(err)
			}
			distribution := New(5 * time.Second)
			err := distribution.download(ctx, server.URL, dest)
			want := "original cache"
			switch name {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
				want = "runner archive"
			case "truncated":
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal("truncated response was not reported", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("download did not preserve cancellation", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
					t.Fatal("HTTP failure was not reported", err)
				}
			}
			if data, err := os.ReadFile(dest); err != nil || string(data) != want {
				t.Fatal("download published incomplete data or lost the original cache", string(data), err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(dest) {
				t.Fatal("download left temporary files", entries, err)
			}
		})
	}
}

func TestArchiveExtractionRejectsUnsafeEntries(t *testing.T) {
	for name, entry := range map[string]archiveEntry{
		"parent traversal": {name: "../outside", body: "bad"},
		"absolute":         {name: "/outside", body: "bad"},
		"symlink escape":   {name: "escape", kind: tar.TypeSymlink, link: "../../outside"},
		"absolute link":    {name: "escape", kind: tar.TypeSymlink, link: "/outside"},
		"device":           {name: "device", kind: tar.TypeChar},
		"hardlink":         {name: "hardlink", kind: tar.TypeLink, link: "/outside"},
	} {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(testDirectory(t), "runner.tar.gz")
			writeArchive(t, archive, []archiveEntry{entry})
			if err := extractRunner(t.Context(), archive, filepath.Join(testDirectory(t), "runner")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestArchiveSymlinkResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []archiveEntry
		wantErr bool
	}{
		{"forward link", []archiveEntry{
			{name: "bin/link", kind: tar.TypeSymlink, link: "Runner.Listener"},
		}, false},
		{"safe chain", []archiveEntry{
			{name: "link", kind: tar.TypeSymlink, link: "hop"},
			{name: "hop", kind: tar.TypeSymlink, link: "bin/Runner.Listener"},
		}, false},
		{"dotdot escape", []archiveEntry{
			{name: "pivot", kind: tar.TypeSymlink, link: "."},
			{name: "nested/link", kind: tar.TypeSymlink, link: "../pivot/../secret"},
		}, true},
		{"cycle", []archiveEntry{
			{name: "link", kind: tar.TypeSymlink, link: "hop"},
			{name: "hop", kind: tar.TypeSymlink, link: "link"},
		}, true},
		{"dangling", []archiveEntry{
			{name: "link", kind: tar.TypeSymlink, link: "missing"},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := testDirectory(t)
			secret := filepath.Join(base, "secret")
			if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "runner.tar.gz")
			entries := []archiveEntry{
				{name: "bin", kind: tar.TypeDir}, {name: "nested", kind: tar.TypeDir},
			}
			entries = append(entries, tc.entries...)
			entries = append(entries, archiveEntry{name: "bin/Runner.Listener", body: "#!/bin/sh\nexit 0\n"})
			writeArchive(t, archive, entries)
			dest := filepath.Join(base, "runner")
			err := extractRunner(t.Context(), archive, dest)
			if (err != nil) != tc.wantErr {
				t.Fatalf("extraction error %v, want error=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), "symlink") {
					t.Fatal("missing link diagnostic", err)
				}
				info, err := os.Stat(dest)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatal("failed extraction exposed the directory", info, err)
				}
			} else {
				data, err := os.ReadFile(filepath.Join(dest, tc.entries[0].name))
				if err != nil || string(data) != "#!/bin/sh\nexit 0\n" {
					t.Fatal("safe extracted link is not accessible", err)
				}
			}
			data, err := os.ReadFile(secret)
			info, statErr := os.Stat(secret)
			if err != nil || statErr != nil || string(data) != "private" || info.Mode().Perm() != 0600 {
				t.Fatal("extraction changed the outside file", err, statErr)
			}
		})
	}
}

func TestOfficialReleaseDigestAndCanceledPreparation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/tags/v2.338.0" {
			t.Errorf("unexpected URL %s", r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]any{"name": "runner.tar.gz", "digest": "sha256:" + strings.Repeat("a", 64)}}})
	}))
	defer server.Close()
	distribution := New(10 * time.Minute)
	distribution.apiBase = server.URL
	digest, err := distribution.releaseDigest(t.Context(), "2.338.0", "runner.tar.gz")
	if err != nil || digest != strings.Repeat("a", 64) {
		t.Fatal(digest, err)
	}
	if _, err := distribution.releaseDigest(t.Context(), "2.338.0", "missing.tar.gz"); err == nil {
		t.Fatal("missing official checksum accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := testConfig(t)
	if err := distribution.Prepare(ctx, c, filepath.Join(testDirectory(t), "runner")); err == nil {
		t.Fatal("canceled preparation continued")
	}
}

func TestRunnerPlatforms(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch, want string
		wantErr            bool
	}{
		{goos: "darwin", goarch: "arm64", want: "osx-arm64"},
		{goos: "darwin", goarch: "amd64", want: "osx-x64"},
		{goos: "linux", goarch: "amd64", want: "linux-x64"},
		{goos: "linux", goarch: "arm64", want: "linux-arm64"},
		{goos: "windows", goarch: "amd64", wantErr: true},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			got, err := runnerPlatform(tc.goos, tc.goarch)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestPreparationWaitingForCacheCanBeCanceled(t *testing.T) {
	d := New(10 * time.Minute)
	d.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := d.Prepare(ctx, testConfig(t), filepath.Join(testDirectory(t), "runner")); err == nil {
		t.Fatal("canceled cache wait succeeded")
	}

	if time.Since(start) > time.Second {
		t.Fatal("cache serialization prevented drain cancellation")
	}
}

func TestExtractionDoesNotHoldCacheGate(t *testing.T) {
	c := testConfig(t)
	seedDistribution(t, c, "#!/bin/sh\nexit 0\n")
	d := New(time.Minute)
	first, second := filepath.Join(testDirectory(t), "first"), filepath.Join(testDirectory(t), "second")
	started, release := make(chan struct{}), make(chan struct{})
	d.extract = func(ctx context.Context, archive, dest string) error {
		if dest == first {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return extractRunner(ctx, archive, dest)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	result := make(chan error, 1)
	workers.Go(func() { result <- d.Prepare(ctx, c, first) })
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first extraction did not start", ctx.Err())
	}
	if err := d.Prepare(ctx, c, second); err != nil {
		t.Fatal("another extraction retained the cache gate", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
