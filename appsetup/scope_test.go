package appsetup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

func TestSetupCreatesConfigAndResumes(t *testing.T) {
	for _, scope := range []string{"owner/repo", "owner"} {
		t.Run(scope, func(t *testing.T) {
			f := setupFixture(t, scope, "Organization")
			f.options.ID = ""
			f.options.Config = filepath.Join(filepath.Dir(f.options.Config), "new", "config.yaml")
			result, err := f.run(t.Context(), f.options, nil, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.ReplaceAll(scope, "/", "-")
			config, err := ghalet.LoadConfig(f.options.Config)
			if err != nil || len(config.Targets) != 1 || config.Targets[0].ID != want ||
				config.Targets[0].InstallationID != 123 || result.Target != want {
				t.Fatal("new configuration did not use the derived ID", config, err)
			}
			info, err := os.Stat(f.options.Config)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("new configuration is not private", err)
			}
			second, err := f.run(t.Context(), f.options, nil, io.Discard)
			if err != nil || second.KeyPath != result.KeyPath || f.conversions.Load() != 1 {
				t.Fatal("repeated scope setup created another App", err)
			}
			config, err = ghalet.LoadConfig(f.options.Config)
			if err != nil || len(config.Targets) != 1 {
				t.Fatal("repeated scope setup duplicated the target", err)
			}
		})
	}
}

func TestSetupDeclineDoesNotCreateConfig(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Config = filepath.Join(filepath.Dir(f.options.Config), "new", "config.yaml")
	f.options.ID, f.options.Yes = "", false
	first, err := f.run(t.Context(), f.options, confirmationInput(t, "n\n"), io.Discard)
	if err == nil {
		t.Fatal("declined setup succeeded")
	}
	if _, err := os.Stat(f.options.Config); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("declined setup created configuration", err)
	}
	f.options.Yes = true
	second, err := f.run(t.Context(), f.options, nil, io.Discard)
	if err != nil || second.KeyPath != first.KeyPath || f.conversions.Load() != 1 {
		t.Fatal("new configuration could not resume after declining", err)
	}
}

func TestSystemdAppSetup(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Systemd = true
	f.options.Config = filepath.Join(filepath.Dir(f.options.Config), "system.yaml")
	directory, err := credentialstore.PrepareDirectory()
	if err != nil {
		t.Fatal(err)
	}
	f.prepare = func(context.Context) (string, error) { return directory, nil }
	saves, reads := 0, 0
	f.saveKey = func(ctx context.Context, options credentialstore.Options, input io.Reader) (credentialstore.Result, error) {
		saves++
		if !options.NoDefault || !options.NoReplace || options.Name != "build" {
			t.Fatal("systemd setup did not use isolated, exclusive key registration", options)
		}
		data, err := io.ReadAll(input)
		if err != nil {
			return credentialstore.Result{}, err
		}
		path := filepath.Join(directory, options.Name+".cred")
		if err := fshelper.WriteFileAtomic(path, []byte(base64.StdEncoding.EncodeToString(data)), 0600); err != nil {
			return credentialstore.Result{}, err
		}
		return credentialstore.Result{EncryptedPath: path, RuntimePath: "/run/credentials/ghalet.service/" + options.Name}, nil
	}
	f.readKey = func(ctx context.Context, path string) ([]byte, error) {
		reads++
		if path != "/run/credentials/ghalet.service/build" {
			t.Fatal("setup tried to read a non-systemd key path", path)
		}
		data, err := os.ReadFile(filepath.Join(directory, "build.cred"))
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(string(data))
	}
	var output bytes.Buffer
	first, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil {
		t.Fatal(err, output.String())
	}
	second, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil || saves != 1 || reads != 4 || first.KeyPath != second.KeyPath || f.conversions.Load() != 1 {
		t.Fatal("systemd setup did not reuse the encrypted key", saves, reads, err)
	}
	config, err := ghalet.LoadConfig(f.options.Config)
	if err != nil || config.Targets[0].Credential.PrivateKeyFile != first.KeyPath ||
		config.Targets[0].InstallationID != 123 || config.Runner.User != "ghalet-runner" ||
		config.Logs.Root != "/var/log/ghalet/runners" {
		t.Fatal("new systemd config does not use package service settings", config, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "build.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("systemd setup wrote a plaintext PEM", err)
	}
	if strings.Contains(output.String(), "PRIVATE KEY") || !strings.Contains(output.String(), "Encrypted key:") {
		t.Fatal("systemd output disclosed or misidentified the key")
	}
}

func TestSetupPreflightFailureDoesNotCreateApp(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Systemd = true
	f.prepare = func(context.Context) (string, error) { return "", errors.New("systemd is unavailable") }
	if _, err := f.run(t.Context(), f.options, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "systemd is unavailable") {
		t.Fatal(err)
	}
	if f.ownerCalls.Load() != 0 || f.conversions.Load() != 0 {
		t.Fatal("systemd preflight failure created an App")
	}
}
