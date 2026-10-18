package ghalet

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type archiveEntry struct {
	name string
	body string
	link string
	kind byte
	mode int64
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

func seedDistribution(t *testing.T, c *Config, script string) {
	t.Helper()
	for _, dir := range []string{c.Runner.CacheRoot, c.Runner.DistributionRoot} {
		if err := safeDirectory(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	platform, err := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	name := "actions-runner-" + platform + "-" + c.Runner.Version + ".tar.gz"
	archive := filepath.Join(c.Runner.CacheRoot, name)
	writeArchive(t, archive, []archiveEntry{{name: "bin/Runner.Listener", body: script}})
	digest, err := fileDigest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Runner.DistributionRoot, name+".sha256"), []byte(digest+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedDistributionAndIndependentExecutions(t *testing.T) {
	c := testConfig(t)
	seedDistribution(t, c, "#!/bin/sh\nexit 0\n")
	distribution := newDistribution()
	for _, run := range []string{"first", "second"} {
		dest := filepath.Join(testDirectory(t), run)
		if err := distribution.prepare(t.Context(), c, dest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "bin/Runner.Listener"), []byte("job modification"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	platform, _ := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	archive := filepath.Join(c.Runner.CacheRoot, "actions-runner-"+platform+"-"+c.Runner.Version+".tar.gz")
	os.WriteFile(archive, []byte("corrupt"), 0600)
	if err := distribution.prepare(t.Context(), c, filepath.Join(testDirectory(t), "third")); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal("corrupt cache was accepted or silently replaced:", err)
	}
}

func TestArchiveExtractionRejectsUnsafeEntries(t *testing.T) {
	for name, entry := range map[string]archiveEntry{
		"parent traversal": {name: "../outside", body: "bad"},
		"absolute":         {name: "/outside", body: "bad"},
		"symlink escape":   {name: "escape", kind: tar.TypeSymlink, link: "../../outside"},
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

func TestOfficialReleaseDigestAndCanceledPreparation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/actions/runner/releases/tags/v2.338.0" {
			t.Errorf("unexpected URL %s", r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{"assets": []any{map[string]any{"name": "runner.tar.gz", "digest": "sha256:" + strings.Repeat("a", 64)}}})
	}))
	defer server.Close()
	distribution := newDistribution()
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
	if err := distribution.prepare(ctx, c, filepath.Join(testDirectory(t), "runner")); err == nil {
		t.Fatal("canceled preparation continued")
	}
}

func TestRunnerPlatforms(t *testing.T) {
	for _, value := range [][3]string{{"darwin", "arm64", "osx-arm64"}, {"darwin", "amd64", "osx-x64"}, {"linux", "amd64", "linux-x64"}, {"linux", "arm64", "linux-arm64"}} {
		actual, err := runnerPlatform(value[0], value[1])
		if err != nil || actual != value[2] {
			t.Fatal(value, actual, err)
		}
	}
	if _, err := runnerPlatform("windows", "amd64"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}

func TestPreparationWaitingForCacheCanBeCanceled(t *testing.T) {
	d := newDistribution()
	d.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := d.prepare(ctx, testConfig(t), filepath.Join(testDirectory(t), "runner")); err == nil {
		t.Fatal("canceled cache wait succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cache serialization prevented drain cancellation")
	}
}
