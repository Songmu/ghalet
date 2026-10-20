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
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
)

func fileOptions(t *testing.T) (string, Options) {
	t.Helper()
	root := testDirectory(t)
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("# retained\ntargets:\n  - id: build\n    scope: owner/repo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "credentials"), Options{Name: "default", Type: "pat", Stdin: true, Config: path}
}

func TestFileRegistrationAndRotation(t *testing.T) {
	directory, options := fileOptions(t)
	first, err := setFile(t.Context(), directory, options, strings.NewReader("first-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := ghalet.LoadConfig(options.Config)
	if err != nil || config.Targets[0].Credential.TokenFile != first.RuntimePath {
		t.Fatal("credential default not configured", config, err)
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
