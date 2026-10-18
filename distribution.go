package ghalet

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type runnerDistribution struct {
	gate    chan struct{}
	client  *http.Client
	apiBase string
}

func newDistribution() *runnerDistribution {
	return &runnerDistribution{gate: make(chan struct{}, 1), client: &http.Client{Timeout: preparationTimeout}, apiBase: "https://api.github.com"}
}

func runnerPlatform(goos, arch string) (string, error) {
	switch goos {
	case "darwin":
		goos = "osx"
	case "linux":
	default:
		return "", fmt.Errorf("unsupported runner OS %s", goos)
	}
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
	case "arm":
		if goos != "linux" {
			return "", fmt.Errorf("unsupported runner architecture %s/%s", goos, arch)
		}
	default:
		return "", fmt.Errorf("unsupported runner architecture %s/%s", goos, arch)
	}
	return goos + "-" + arch, nil
}

func (d *runnerDistribution) prepare(ctx context.Context, c *Config, dest string) error {
	select {
	case d.gate <- struct{}{}:
		defer func() { <-d.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	platform, err := runnerPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	name := "actions-runner-" + platform + "-" + c.Runner.Version + ".tar.gz"
	for _, dir := range []string{c.Runner.CacheRoot, c.Runner.DistributionRoot} {
		if err := safeDirectory(dir, 0700); err != nil {
			return err
		}
	}
	archive := filepath.Join(c.Runner.CacheRoot, name)
	checksumFile := filepath.Join(c.Runner.DistributionRoot, name+".sha256")
	checksum, err := os.ReadFile(checksumFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	expected := strings.TrimSpace(string(checksum))
	if expected == "" {
		expected, err = d.releaseDigest(ctx, c.Runner.Version, name)
		if err != nil {
			return err
		}
		if err := atomicFile(checksumFile, []byte(expected+"\n"), 0600); err != nil {
			return err
		}
	}
	if len(expected) != 64 {
		return errors.New("invalid official SHA-256 digest")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("invalid official SHA-256 digest")
	}
	if _, err := os.Stat(archive); errors.Is(err, os.ErrNotExist) {
		url := "https://github.com/actions/runner/releases/download/v" + c.Runner.Version + "/" + name
		if err := d.download(ctx, url, archive); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	actual, err := fileDigest(archive)
	if err != nil {
		return err
	}
	if actual != expected {
		// Do not reuse or silently repair an unverified cache.
		return fmt.Errorf("runner checksum mismatch for %s; remove the corrupt cache and retry", archive)
	}
	return extractRunner(ctx, archive, dest)
}

func (d *runnerDistribution) releaseDigest(ctx context.Context, version, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.apiBase+"/repos/actions/runner/releases/tags/v"+version, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ghalet/"+version)
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runner release metadata: HTTP %d", resp.StatusCode)
	}
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&release); err != nil {
		return "", err
	}
	for _, asset := range release.Assets {
		if asset.Name == name && strings.HasPrefix(asset.Digest, "sha256:") {
			return strings.TrimPrefix(asset.Digest, "sha256:"), nil
		}
	}
	return "", fmt.Errorf("release %s has no official SHA-256 digest for %s", version, name)
}

func atomicFile(dest string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dest)
}

func (d *runnerDistribution) download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download runner: HTTP %d", resp.StatusCode)
	}
	file, err := os.CreateTemp(filepath.Dir(dest), ".runner-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, copyErr := io.Copy(file, resp.Body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), dest)
}

func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("cache must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func extractRunner(ctx context.Context, archive, dest string) error {
	if err := safeDirectory(dest, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		mode := os.FileMode(header.Mode) & 0777
		switch header.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, mode|0700)
		case tar.TypeReg:
			if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return err
			}
			var output *os.File
			output, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0600)
			if err == nil {
				_, copyErr := io.Copy(output, reader)
				err = errors.Join(copyErr, output.Close())
			}
		case tar.TypeSymlink:
			target := path.Clean(path.Join(path.Dir(name), header.Linkname))
			if path.IsAbs(header.Linkname) || !filepath.IsLocal(target) {
				return fmt.Errorf("unsafe archive symlink %s", name)
			}
			err = root.Symlink(header.Linkname, name)
		default:
			return fmt.Errorf("unsupported archive entry type %d for %s", header.Typeflag, name)
		}
		if err != nil {
			return err
		}
	}
	listener, err := root.Stat("bin/Runner.Listener")
	if err != nil || !listener.Mode().IsRegular() || listener.Mode().Perm()&0100 == 0 {
		return errors.New("runner archive is missing executable bin/Runner.Listener")
	}
	return nil
}
