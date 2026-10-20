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

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/credentialstore"
)

func TestFileCredentialCLI(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	for _, global := range []bool{false, true} {
		root := testDirectory(t)
		t.Setenv("XDG_STATE_HOME", root)
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
		loaded, err := ghalet.LoadConfig(config)
		if err != nil || loaded.Defaults.Credential.TokenFile != filepath.Join(root, "ghalet", "credentials", "default.pat") {
			t.Fatal("credential was not configured", loaded, err)
		}
	}
}

func TestFileCredentialHelp(t *testing.T) {
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
