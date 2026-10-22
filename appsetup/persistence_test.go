package appsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

func TestSetupAliasesShareConfigurationLock(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	before := []byte("targets: []\n")
	if err := os.WriteFile(f.options.Config, before, 0600); err != nil {
		t.Fatal(err)
	}
	var preparations []preparedConfig
	var directories []string
	for _, name := range []string{"first", "second"} {
		options := f.options
		options.Config = filepath.Join(filepath.Dir(f.options.Config), name+".yaml")
		options.ScaleSet = name
		if err := os.Symlink(f.options.Config, options.Config); err != nil {
			t.Fatal(err)
		}
		prepared, err := setupConfig(options)
		if err != nil {
			t.Fatal(err)
		}
		preparations = append(preparations, prepared)
		directory := filepath.Join(filepath.Dir(f.options.Config), name+"-credentials")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		directories = append(directories, directory)
	}
	if preparations[0].managedPath == preparations[1].managedPath {
		t.Fatal("test needs independent managed files")
	}
	s := service{readKey: func(context.Context, string) ([]byte, error) { return []byte("verified"), nil }}
	verified := sha256.Sum256([]byte("verified"))
	commitBoth := func() []error {
		results := make(chan error, 2)
		start := make(chan struct{})
		for i, prepared := range preparations {
			go func(i int, prepared preparedConfig) {
				<-start
				results <- s.commitSetup(t.Context(), prepared, directories[i], "", verified, []byte("{}\n"))
			}(i, prepared)
		}
		close(start)
		return []error{<-results, <-results}
	}
	lock, err := fshelper.LockFile(f.options.Config + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	for _, err := range commitBoth() {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatal("configuration alias bypassed the canonical lock", err)
		}
	}
	lock.Close()
	successes := 0
	for _, err := range commitBoth() {
		if err == nil {
			successes++
		}
	}
	current, err := os.ReadFile(f.options.Config)
	if successes != 1 || err != nil ||
		(!bytes.Equal(current, preparations[0].proposed) && !bytes.Equal(current, preparations[1].proposed)) {
		t.Fatal("concurrent alias setups lost a successful registration", successes, err)
	}
}

type cancelAfterConversion struct {
	http.RoundTripper
	cancel context.CancelFunc
}

func (transport cancelAfterConversion) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.RoundTripper.RoundTrip(request)
	if err == nil && strings.HasPrefix(request.URL.Path, "/app-manifests/") {
		response.Body = cancelAfterEOF{ReadCloser: response.Body, cancel: transport.cancel}
	}
	return response, err
}

type cancelAfterEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body cancelAfterEOF) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		body.cancel()
	}
	return n, err
}

func TestCancellationAfterConversionRetainsCredentials(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	before, err := os.ReadFile(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transport := f.api.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	f.api.http.Transport = cancelAfterConversion{RoundTripper: transport, cancel: cancel}
	var output bytes.Buffer
	first, err := f.run(ctx, f.options, nil, &output)
	if !errors.Is(err, context.Canceled) || f.conversions.Load() != 1 {
		t.Fatal("setup was not canceled after the one-time code conversion", err)
	}
	key, err := readPrivate(first.KeyPath)
	if err != nil || !bytes.Equal(bytes.TrimSpace(key), bytes.TrimSpace([]byte(f.pem))) {
		t.Fatal("cancellation after conversion lost the App key", err)
	}
	state, err := loadApp(first.MetadataPath)
	if err != nil || state.ID != 77 || state.ClientID != "Iv1.test" || state.KeyPath != first.KeyPath {
		t.Fatal("cancellation after conversion lost resumption metadata", err)
	}
	for _, path := range []string{first.KeyPath, first.MetadataPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("retained credentials are not private", err)
		}
	}
	after, err := os.ReadFile(f.options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled setup changed configuration", err)
	}
	if f.installed.Load() {
		t.Fatal("canceled setup continued to installation")
	}
	second, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil || second.InstallationID != 123 || second.KeyPath != first.KeyPath ||
		f.conversions.Load() != 1 || f.ownerCalls.Load() != 1 {
		t.Fatal("resume did not reuse the saved App and key", err)
	}
	if _, err := os.Stat(first.KeyPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resuming unexpectedly rotated the key", err)
	}
}

func TestSetupResumesAfterTrustedBaseMigration(t *testing.T) {
	for _, movedDirectory := range []string{"base", "credentials"} {
		t.Run(movedDirectory, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			base := filepath.Join(filepath.Dir(f.options.Config), "service-state")
			t.Setenv("STATE_DIRECTORY", base)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			transport := f.api.http.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			f.api.http.Transport = cancelAfterConversion{RoundTripper: transport, cancel: cancel}
			first, err := f.run(ctx, f.options, nil, io.Discard)
			if !errors.Is(err, context.Canceled) || f.conversions.Load() != 1 {
				t.Fatal("setup did not retain a single converted App", err)
			}
			saved, err := loadApp(first.MetadataPath)
			if err != nil || saved.KeyPath != first.KeyPath {
				t.Fatal("setup did not persist the pre-migration key path", err)
			}
			source := base
			if movedDirectory == "credentials" {
				source = filepath.Dir(first.KeyPath)
			}
			moved, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			moved = filepath.Join(moved, "migrated")
			if err := os.Rename(source, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, source); err != nil {
				t.Fatal(err)
			}
			second, err := f.run(t.Context(), f.options, nil, io.Discard)
			if movedDirectory == "credentials" {
				if err == nil {
					t.Fatal("setup resolved a managed credential-directory symlink")
				}
				return
			}
			want := filepath.Join(moved, "credentials", f.options.credentialName+".pem")
			if err != nil || second.KeyPath != want || second.InstallationID != 123 ||
				f.conversions.Load() != 1 || f.ownerCalls.Load() != 1 {
				t.Fatal("resumption did not reuse the migrated App and canonical key", second, err)
			}
			config, err := settings.LoadConfig(f.options.Config)
			if err != nil || config.Targets[0].Credential.PrivateKeyFile != want {
				t.Fatal("configuration did not use the canonical key", config, err)
			}
			if _, err := os.Stat(want + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("resumption rotated the migrated key", err)
			}
		})
	}
}

func TestConversionWaitsForCredentialStoreLock(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	before, err := os.ReadFile(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := credentialstore.PrepareDirectory()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := credentialstore.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transport := f.api.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	f.api.http.Transport = cancelAfterConversion{RoundTripper: transport, cancel: cancel}
	attempted := make(chan struct{})
	released := make(chan struct{})
	release := sync.OnceFunc(func() {
		lock.Close()
		close(released)
	})
	defer release()
	var busyAttempts atomic.Int32
	saveKey := f.saveKey
	f.saveKey = func(ctx context.Context, options credentialstore.Options, input io.Reader) (credentialstore.Result, error) {
		if ctx.Err() != nil {
			t.Error("credential persistence inherited cancellation")
		}
		result, err := saveKey(ctx, options, input)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if busyAttempts.Add(1) == 1 {
				close(attempted)
			}
			<-released
		}
		return result, err
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := f.run(ctx, f.options, nil, io.Discard)
		done <- err
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("setup did not exit after releasing the store lock")
		}
	})
	select {
	case <-attempted:
	case err := <-done:
		t.Fatal("setup returned before encountering the held store lock", err)
	case <-time.After(5 * time.Second):
		t.Fatal("setup did not attempt credential persistence")
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("setup abandoned converted credentials on lock contention", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setup did not save credentials after the store lock was released")
	}
	keyPath := filepath.Join(directory, f.options.credentialName+".pem")
	key, err := readPrivate(keyPath)
	if err != nil || !bytes.Equal(bytes.TrimSpace(key), bytes.TrimSpace([]byte(f.pem))) {
		t.Fatal("store lock contention lost the original converted App key", err)
	}
	state, err := loadApp(filepath.Join(directory, f.options.credentialName+".app.json"))
	if err != nil || state.ID != 77 || state.ClientID != "Iv1.test" || state.KeyPath != keyPath {
		t.Fatal("store lock contention lost resumption metadata", err)
	}
	after, err := os.ReadFile(f.options.Config)
	if err != nil || !bytes.Equal(before, after) || f.installed.Load() {
		t.Fatal("canceled persistence continued installation or changed configuration", err)
	}
	result, err := f.run(t.Context(), f.options, nil, io.Discard)
	if err != nil || result.InstallationID != 123 || result.KeyPath != keyPath ||
		f.conversions.Load() != 1 || f.ownerCalls.Load() != 1 || busyAttempts.Load() != 1 {
		t.Fatal("resume did not reuse the converted App and key", err)
	}
	if _, err := os.Stat(keyPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resuming unexpectedly rotated the key", err)
	}
}
