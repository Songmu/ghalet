package cmd

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppHelp(t *testing.T) {
	for _, args := range [][]string{
		{"app", "--help"}, {"app", "setup", "--help"},
		{"--config", "custom.yaml", "app", "setup", "--help"},
		{"systemd", "app", "--help"}, {"systemd", "app", "setup", "--help"},
		{"--config", "custom.yaml", "systemd", "app", "setup", "--help"},
	} {
		var output bytes.Buffer
		if err := Run(t.Context(), args, &output, &output); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "app setup --scope") {
			t.Fatal(output.String())
		}
	}
}

func TestAppOptions(t *testing.T) {
	options, err := parseAppOptions([]string{"--scope", "owner/repo", "--id", "build", "--listen", "127.0.0.1:8765", "--no-browser", "--yes"}, "selected.yaml", &bytes.Buffer{}, false)
	if err != nil || options.Scope != "owner/repo" || options.ID != "build" || options.Config != "selected.yaml" ||
		options.Listen != "127.0.0.1:8765" || !options.NoBrowser || !options.Yes || options.Systemd {
		t.Fatal(options, err)
	}
	for _, args := range [][]string{{}, {"work", "extra"}, {"--yes"}, {"--scope", "owner/repo", "work"}, {"--scope", "owner/repo", "--id", "../escape"}} {
		if _, err := parseAppOptions(args, "", &bytes.Buffer{}, false); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	for _, config := range []string{"", "selected.yaml"} {
		options, err := parseAppOptions([]string{"--scope", "owner"}, config, &bytes.Buffer{}, true)
		want := config
		if want == "" {
			want = "/etc/ghalet/config.yaml"
		}
		if err != nil || !options.Systemd || !options.NoBrowser || options.Config != want {
			t.Fatal(options, err)
		}
	}
	options, err = parseAppOptions([]string{"--scope", "owner", "--config", "override.yaml"}, "selected.yaml", &bytes.Buffer{}, true)
	if err != nil || options.Config != "override.yaml" {
		t.Fatal(options, err)
	}
}

func TestAppScopeRequired(t *testing.T) {
	for _, args := range [][]string{
		{"app", "setup"}, {"systemd", "app", "setup"},
		{"--config", "missing.yaml", "app", "setup"},
		{"--config", "missing.yaml", "systemd", "app", "setup"},
	} {
		var output bytes.Buffer
		if err := Run(t.Context(), args, &output, &output); err == nil || !strings.Contains(err.Error(), "--scope is required") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestAppConfigFlagRouting(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "invalid.yaml")
	if err := os.WriteFile(path, []byte("invalid: configuration\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"app", "setup", "--scope", "owner/repo", "--config", path},
		{"--config", path, "app", "setup", "--scope", "owner/repo"},
		{"systemd", "app", "setup", "--scope", "owner/repo", "--config", path},
		{"--config", path, "systemd", "app", "setup", "--scope", "owner/repo"},
	} {
		var output bytes.Buffer
		if err := Run(t.Context(), args, &output, &output); err == nil || !strings.Contains(err.Error(), "decode config") {
			t.Fatalf("%v did not select the configuration: %v", args, err)
		}
	}
}

func TestLegacyAuthSetupRemoved(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"auth", "setup", "project-app"}, &output, &output); err == nil {
		t.Fatal("legacy auth setup command is still accepted")
	}
	if strings.Contains(output.String(), "auth setup") {
		t.Fatal("root help still advertises the old command")
	}
}
