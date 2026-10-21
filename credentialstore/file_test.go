package credentialstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

func fileOptions(t *testing.T) (string, Options) {
	t.Helper()
	root := testDirectory(t)
	t.Setenv("STATE_DIRECTORY", filepath.Join(root, "state"))
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("# retained\nmanagedFile: managed.yaml\ntargets:\n  - scaleSet: build\n    scope: owner/repo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "credentials"), Options{Name: "default", Type: "pat", Stdin: true, Config: path}
}

func TestFileRegistrationAndRotation(t *testing.T) {
	directory, options := fileOptions(t)
	staticBefore, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := setFile(t.Context(), directory, options, strings.NewReader("first-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := settings.LoadConfig(options.Config)
	if err != nil || config.Targets[0].Credential.TokenFile != first.RuntimePath {
		t.Fatal("credential default not configured", config, err)
	}
	staticAfter, err := os.ReadFile(options.Config)
	if err != nil || !bytes.Equal(staticBefore, staticAfter) {
		t.Fatal("registration rewrote static configuration", err)
	}
	for path, mode := range map[string]os.FileMode{directory: 0700, first.RuntimePath: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("incorrect permissions", path, info, err)
		}
	}
	options.NoDefault = true
	before, _ := os.ReadFile(options.Config)
	second, err := setFile(t.Context(), directory, options, strings.NewReader("second-secret"))
	if err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(second.PreviousPath)
	current, _ := os.ReadFile(first.RuntimePath)
	after, _ := os.ReadFile(options.Config)
	if string(backup) != "first-secret" || string(current) != "second-secret" || !bytes.Equal(before, after) {
		t.Fatal("rotation lost backup or modified configuration")
	}
	input := strings.NewReader("third-secret")
	if _, err := setFile(t.Context(), directory, options, input); err == nil || input.Len() != len("third-secret") {
		t.Fatal("unresolved backup was not rejected before input", err)
	}
}

func TestFileCredentialsWithTrustedBaseAlias(t *testing.T) {
	_, options := fileOptions(t)
	base := filepath.Dir(options.Config)
	link := filepath.Join(testDirectory(t), "layout")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	options.Config = filepath.Join(link, "config.yaml")
	t.Setenv("STATE_DIRECTORY", filepath.Join(link, "state"))
	result, err := SetFile(t.Context(), options, strings.NewReader("synthetic-secret"))
	want := filepath.Join(base, "state", "credentials", "default.pat")
	if err != nil || result.RuntimePath != want {
		t.Fatal("credential path was not canonical", result, err)
	}
	config, err := settings.LoadConfig(options.Config)
	if err != nil || config.Path != options.Config || config.Targets[0].Credential.TokenFile != want {
		t.Fatal("credential registration changed config identity or stored an alias", config, err)
	}
	directory, err := Directory()
	if err != nil {
		t.Fatal(err)
	}
	outside := testDirectory(t)
	if err := os.Rename(directory, filepath.Join(outside, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, directory); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "default.pat")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFile(t.Context(), options, strings.NewReader("replacement")); err == nil {
		t.Fatal("symlinked managed credential directory accepted")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("credential update escaped its managed directory", err)
	}
}

func TestCredentialWritersHonorConfigLock(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "systemd"}[encrypted], func(t *testing.T) {
			store, options, _ := testCredentialStore(t)
			before, err := os.ReadFile(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			managedPath, err := settings.ManagedPath(options.Config, "managed.yaml")
			if err != nil {
				t.Fatal(err)
			}
			lock, err := fshelper.LockFile(managedPath + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			link := filepath.Join(testDirectory(t), "config-layout")
			if err := os.Symlink(filepath.Dir(options.Config), link); err != nil {
				t.Fatal(err)
			}
			options.Config = filepath.Join(link, filepath.Base(options.Config))
			var setErr error
			if encrypted {
				_, setErr = store.set(t.Context(), options, strings.NewReader("synthetic-secret"))
			} else {
				_, setErr = setFile(t.Context(), store.directory, options, strings.NewReader("synthetic-secret"))
			}
			if setErr == nil {
				t.Fatal("credential writer ignored the configuration lock")
			}
			current, err := os.ReadFile(options.Config)
			if err != nil || !bytes.Equal(current, before) {
				t.Fatal("configuration was changed", err)
			}
			for _, name := range []string{"default.pat", "default.cred", "default.cred.previous"} {
				if _, err := os.Stat(filepath.Join(store.directory, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed configuration update modified credentials", name, err)
				}
			}
			options.NoDefault = true
			if encrypted {
				_, setErr = store.set(t.Context(), options, strings.NewReader("synthetic-secret"))
			} else {
				_, setErr = setFile(t.Context(), store.directory, options, strings.NewReader("synthetic-secret"))
			}
			if setErr != nil {
				t.Fatal("--no-default unnecessarily required the configuration lock", setErr)
			}
		})
	}
}

func TestFileCredentialFailuresPreserveExisting(t *testing.T) {
	for _, kind := range []string{"empty", "whitespace", "large", "invalid-app", "canceled", "missing-input", "prompt"} {
		t.Run(kind, func(t *testing.T) {
			directory, options := fileOptions(t)
			result, err := setFile(t.Context(), directory, options, strings.NewReader("old-secret"))
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			input := strings.NewReader("")
			switch kind {
			case "whitespace":
				input = strings.NewReader("bad token")
			case "large":
				input = strings.NewReader(strings.Repeat("a", maxCredentialSize+1))
			case "invalid-app":
				options.Type = "github-app"
				options.NoDefault = true
				input = strings.NewReader("invalid-key")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "prompt":
				options.Prompt, options.Stdin = true, false
			}
			var setErr error
			if kind == "missing-input" {
				_, setErr = setFile(ctx, directory, options, nil)
			} else {
				_, setErr = setFile(ctx, directory, options, input)
			}
			if setErr == nil {
				t.Fatal("invalid replacement accepted")
			}
			current, _ := os.ReadFile(result.RuntimePath)
			if string(current) != "old-secret" {
				t.Fatal("failed operation changed credential")
			}
			if _, err := os.Stat(result.RuntimePath + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed operation created a backup", err)
			}
		})
	}
}

func TestFileCredentialRollback(t *testing.T) {
	for _, stage := range []string{"credential-write", "config-write", "restore"} {
		t.Run(stage, func(t *testing.T) {
			directory, options := fileOptions(t)
			original, err := setFile(t.Context(), directory, options, strings.NewReader("old-secret"))
			if err != nil {
				t.Fatal(err)
			}
			path := original.RuntimePath
			previousPath := path + ".previous"
			if err := os.Chmod(path, 0400); err != nil {
				t.Fatal(err)
			}
			oldConfig, err := os.ReadFile(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			writeError := errors.New("injected atomic write failure")
			restoreError := errors.New("injected credential restoration failure")
			var writes []string
			writeAtomic := func(destination string, data []byte, mode os.FileMode) error {
				writes = append(writes, destination)
				fail := (stage == "credential-write" && destination == path) || destination == original.ManagedPath
				restoreFail := stage == "restore" && destination == path && string(data) == "old-secret"
				if fail || restoreFail {
					backup, err := os.ReadFile(previousPath)
					if err != nil || string(backup) != "old-secret" {
						t.Fatal("failure was not injected after recovery backup creation", err)
					}
					if restoreFail {
						return restoreError
					}
					return writeError
				}
				return fshelper.WriteFileAtomic(destination, data, mode)
			}
			result, err := setFileWithWriter(t.Context(), directory, options, strings.NewReader("new-secret"), writeAtomic)
			if !errors.Is(err, writeError) || result != (Result{}) {
				t.Fatal("write failure was not reported correctly", result, err)
			}
			wantWrites := []string{previousPath, path}
			if stage != "credential-write" {
				wantWrites = append(wantWrites, original.ManagedPath, path)
			}
			if !slices.Equal(writes, wantWrites) {
				t.Fatal("unexpected write/restore sequence", writes)
			}
			currentConfig, readErr := os.ReadFile(options.Config)
			if readErr != nil || !bytes.Equal(currentConfig, oldConfig) {
				t.Fatal("failed registration changed configuration", readErr)
			}
			wantSecret, wantMode := "old-secret", os.FileMode(0400)
			if stage == "restore" {
				wantSecret, wantMode = "new-secret", 0600
				if !errors.Is(err, restoreError) ||
					!strings.Contains(err.Error(), "restore "+path+":") ||
					!strings.Contains(err.Error(), "previous credential retained at "+previousPath) {
					t.Fatal("restoration failure or recovery path was not reported", err)
				}
				backup, err := os.ReadFile(previousPath)
				if err != nil || string(backup) != "old-secret" {
					t.Fatal("recovery credential was lost", err)
				}
				info, err := os.Stat(previousPath)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("recovery backup permissions were not private", err)
				}
			} else if _, err := os.Stat(previousPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful rollback did not remove the backup", err)
			}
			current, err := os.ReadFile(path)
			if err != nil || string(current) != wantSecret {
				t.Fatal("incorrect credential after rollback", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != wantMode {
				t.Fatal("incorrect credential permissions after rollback", err)
			}
		})
	}
}

func TestFileCredentialUnsafePaths(t *testing.T) {
	for _, kind := range []string{"directory-mode", "file-mode", "symlink", "backup-symlink", "state-mode"} {
		t.Run(kind, func(t *testing.T) {
			directory, options := fileOptions(t)
			options.NoDefault = true
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "default.pat")
			switch kind {
			case "directory-mode":
				os.Chmod(directory, 0755)
			case "state-mode":
				os.Chmod(filepath.Dir(directory), 0777)
			case "file-mode":
				os.WriteFile(path, []byte("old"), 0644)
			case "symlink":
				os.Symlink(options.Config, path)
			case "backup-symlink":
				os.Symlink(options.Config, path+".previous")
			}
			input := strings.NewReader("secret")
			if _, err := setFile(t.Context(), directory, options, input); err == nil || input.Len() != len("secret") {
				t.Fatal("unsafe path accepted or consumed input", err)
			}
		})
	}
}

func TestSetFileAppAndDirectory(t *testing.T) {
	root := testDirectory(t)
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("XDG_STATE_HOME", root)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	result, err := SetFile(t.Context(), Options{Name: "app", Type: "github-app", Stdin: true, NoDefault: true}, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "ghalet", "credentials", "app.pem")
	if result.RuntimePath != want {
		t.Fatal(result)
	}
	current, _ := os.ReadFile(want)
	if !bytes.Equal(current, bytes.TrimSpace(data)) {
		t.Fatal("key was not preserved")
	}
	stateInfo, err := os.Stat(filepath.Join(root, "ghalet"))
	if err != nil || stateInfo.Mode().Perm() != 0711 {
		t.Fatal("credential registration blocked state-directory traversal", stateInfo, err)
	}
	if _, err := SetFile(t.Context(), Options{Name: "../invalid", Type: "pat", Stdin: true}, strings.NewReader("secret")); err == nil {
		t.Fatal("invalid options accepted")
	}
}

func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
