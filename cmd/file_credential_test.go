package cmd

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/credentialstore"
)

func TestFileCredentialCLI(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("STATE_DIRECTORY", "")
	for _, global := range []bool{false, true} {
		root := testDirectory(t)
		t.Setenv("XDG_STATE_HOME", root)
		t.Setenv("XDG_CACHE_HOME", root)
		config := writeConfig(t)
		input := filepath.Join(root, "input.pat")
		if err := os.WriteFile(input, []byte("file-test-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"credential", "set", "default", "--file", input, "--config", config}
		if global {
			args = []string{"--config", config, "credential", "set", "default", "--file", input}
		}
		var stdout, stderr bytes.Buffer
		if err := Run(t.Context(), args, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout.String()+stderr.String(), "file-test-secret") || strings.Contains(stdout.String(), "Encrypted") {
			t.Fatal("secret leaked or misleading storage description")
		}
		loaded, err := settings.LoadConfig(config)
		if err != nil || loaded.Defaults.Credential.TokenFile != filepath.Join(root, "ghalet", "credentials", "default.pat") {
			t.Fatal("credential was not configured", loaded, err)
		}
	}
}

func TestFileCredentialHelp(t *testing.T) {
	t.Setenv("HOME", "")
	for _, args := range [][]string{
		{"credential", "--help"},
		{"credential", "set", "--help"},
		{"--config", "custom.yaml", "credential", "set", "--help"},
	} {
		var out bytes.Buffer
		if err := Run(t.Context(), args, &out, &out); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Usage: ghalet credential set") {
			t.Fatal(out.String())
		}
	}
}

func TestFileCredentialWithoutHomeOrConfig(t *testing.T) {
	t.Setenv("HOME", "")
	root := testDirectory(t)
	t.Setenv("STATE_DIRECTORY", filepath.Join(root, "state"))
	input := filepath.Join(root, "input.pat")
	if err := os.WriteFile(input, []byte("synthetic-no-home-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"credential", "set", "default", "--file", input, "--no-default"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "state", "credentials", "default.pat"))
	if err != nil || string(stored) != "synthetic-no-home-secret" {
		t.Fatal("credential registration required HOME or a configuration", err)
	}
}

func TestCredentialConfigPaths(t *testing.T) {
	home := testDirectory(t)
	relative, err := filepath.Abs("selected.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, home, config, want string
		encrypted, noDefault     bool
	}{
		{name: "user default", home: home, want: filepath.Join(home, ".config", "ghalet", "config.yaml")},
		{name: "systemd default without HOME", encrypted: true, want: "/etc/ghalet/config.yaml"},
		{name: "relative selection without HOME", config: "selected.yaml", want: relative},
		{name: "explicit selection with no default", config: "selected.yaml", noDefault: true, want: relative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)
			args := []string{"default", "--stdin"}
			if tc.noDefault {
				args = append(args, "--no-default")
			}
			options, err := parseCredentialOptionsFor(args, tc.config, io.Discard, tc.encrypted)
			if err != nil || options.Config != tc.want {
				t.Fatalf("config = %q, %v; want %q", options.Config, err, tc.want)
			}
		})
	}
}

func TestPrintFileCredentialResult(t *testing.T) {
	var out bytes.Buffer
	options := credentialstore.Options{Name: "app", Type: "github-app", Config: "config.yaml"}
	result := credentialstore.Result{RuntimePath: "/state/credentials/app.pem", PreviousPath: "/state/credentials/app.pem.previous"}
	if err := printFileCredentialResult(&out, options, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unencrypted, owner-only") || !strings.Contains(out.String(), "Previous credential") ||
		!strings.Contains(out.String(), "Updated defaults.credential") {
		t.Fatal(out.String())
	}
	if err := printFileCredentialResult(failingWriter{}, options, result); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}
