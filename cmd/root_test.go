package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/logreduct"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../testdata/config/base.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testDirectory(t), "config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigurationValidationCLI(t *testing.T) {
	path := writeConfig(t)
	for _, args := range [][]string{
		{"config", "validate", "--config", path},
		{"--config", path, "config", "validate"},
		{"--config", path, "config", "validate", "--config", path},
	} {
		var output, stderr bytes.Buffer
		if err := Run(t.Context(), args, &output, &stderr); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, stderr.String())
		}
		if output.String() != "Configuration is valid.\n" || stderr.Len() != 0 {
			t.Fatalf("%v: unexpected output %q (%s)", args, output.String(), stderr.String())
		}
	}
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"--version"}, &output, &output); err != nil || !strings.Contains(output.String(), "ghalet v") {
		t.Fatal("version command failed")
	}
	for _, command := range []string{"run", "apply", "status", "gc"} {
		if err := Run(t.Context(), []string{command, "--config", filepath.Join(testDirectory(t), "missing.yaml")}, &output, &output); err == nil {
			t.Fatalf("%s accepted a missing config", command)
		}
	}
}

func TestConfigurationRejectsLegacyScopeFlag(t *testing.T) {
	var output bytes.Buffer
	err := Run(t.Context(), []string{"destroy", "--scope", "repository"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -scope") {
		t.Fatal("legacy --scope flag was not rejected", err)
	}
}

func TestRemovedCommandsAreNotSupported(t *testing.T) {
	t.Setenv("HOME", testDirectory(t))
	for _, args := range [][]string{
		{"drain"},
		{"drain", "--target", "build"},
		{"drain", "--config", "missing.yaml"},
		{"--config", "missing.yaml", "drain", "--target", "build"},
		{"destroy"},
		{"--config", "missing.yaml", "destroy"},
		{"doctor"},
		{"--config", "missing.yaml", "doctor"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, stderr bytes.Buffer
			err := Run(t.Context(), args, &output, &stderr)
			if err == nil || (err.Error() != "a supported command is required" &&
				!strings.Contains(err.Error(), "flag provided but not defined: -target")) {
				t.Fatal("removed command was not rejected", err)
			}
			if output.Len() != 0 || !strings.Contains(stderr.String(), "Usage: ghalet") ||
				strings.Contains(stderr.String(), "drain") || strings.Contains(stderr.String(), "destroy") ||
				strings.Contains(stderr.String(), "doctor") {
				t.Fatal("unexpected output for removed command", output.String(), stderr.String())
			}
		})
	}
}

func TestVersionFlagPlacementDoesNotStartDaemon(t *testing.T) {
	path := writeConfig(t)
	stateHome := testDirectory(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	missing := filepath.Join(filepath.Dir(path), "missing.yaml")
	t.Setenv("GHALET_TEST_TOKEN", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{
		{"--version"},
		{"run", "--version"},
		{"run", "--config", path, "--version"},
		{"--config", path, "run", "--version"},
		{"--config", path, "apply", "--version"},
		{"--config", path, "config", "validate", "--version"},
		{"--config", missing, "run", "--version"},
		{"--version", "run"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, stderr bytes.Buffer
			if err := Run(ctx, args, &output, &stderr); err != nil {
				t.Fatal("version query entered command execution", err, stderr.String())
			}
			if output.String() != cmdName+" v"+ghalet.Version+" (rev:"+ghalet.Revision+")\n" || stderr.Len() != 0 {
				t.Fatal("version query produced unexpected output", output.String(), stderr.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(stateHome, "ghalet", "runs")); !os.IsNotExist(err) {
		t.Fatal("version query touched daemon execution directories", err)
	}
}

func TestHelpAndInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantHelp bool
		wantErr  string
	}{
		{args: []string{"--help"}, wantHelp: true},
		{args: []string{"run", "--help"}, wantHelp: true},
		{args: []string{"config", "validate", "--help"}, wantHelp: true},
		{args: []string{"--config", "missing.yaml", "gc", "--help"}, wantHelp: true},
		{wantErr: "a supported command is required"},
		{args: []string{"unknown"}, wantErr: "a supported command is required"},
		{args: []string{"config"}, wantErr: "usage: ghalet config validate"},
		{args: []string{"config", "unknown"}, wantErr: "usage: ghalet config validate"},
		{args: []string{"status", "extra"}, wantErr: "unexpected arguments: extra"},
		{args: []string{"run", "--unknown"}, wantErr: "flag provided but not defined: -unknown"},
		{args: []string{"gc", "--dry-run=invalid"}, wantErr: "invalid boolean value"},
		{args: []string{"status", "--fail-on-error=invalid"}, wantErr: "invalid boolean value"},
		{args: []string{"run", "--fail-on-error"}, wantErr: "--fail-on-error is only supported by status"},
		{args: []string{"__ghalet_runner_user", "runner", "extra"}, wantErr: "usage: ghalet __ghalet_runner_user"},
		{args: []string{"__ghalet_runner_user", ""}, wantErr: "usage: ghalet __ghalet_runner_user"},
		{args: []string{"__ghalet_runner_guard"}, wantErr: "usage: ghalet __ghalet_runner_guard"},
		{args: []string{"__ghalet_runner_guard", ""}, wantErr: "usage: ghalet __ghalet_runner_guard"},
		{args: []string{"__ghalet_runner_guard", "runner", "extra"}, wantErr: "usage: ghalet __ghalet_runner_guard"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output, stderr bytes.Buffer
			err := Run(t.Context(), tc.args, &output, &stderr)
			if tc.wantHelp {
				if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "Usage: ghalet") {
					t.Fatalf("expected help, got %v (%s)", err, stderr.String())
				}
				if strings.Contains(stderr.String(), "__ghalet_runner") {
					t.Fatal("private runner commands appeared in help")
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want %q", err, tc.wantErr)
			}
			if output.Len() != 0 {
				t.Fatalf("unexpected stdout: %s", output.String())
			}
		})
	}
}

func TestCommandDispatchAndFlagPlacement(t *testing.T) {
	for _, name := range []string{"run", "apply", "validate", "status", "gc"} {
		t.Run(name, func(t *testing.T) {
			var output, stderr bytes.Buffer
			command := []string{name}
			if name == "validate" {
				command = []string{"config", "validate"}
			}
			for _, prefix := range [][]string{nil, {"--config", "before.yaml"}} {
				args := append(append([]string{}, prefix...), command...)
				args = append(args, "--config", "after.yaml", "--dry-run")
				err := Run(t.Context(), args, &output, &stderr)
				want := "after.yaml"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%v: command was not dispatched: %v", args, err)
				}
			}
		})
	}
}

func TestDefaultLoggerRoutesOutput(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	missing := filepath.Join(testDirectory(t), "missing.yaml")
	for _, command := range []string{"run", "gc", "status"} {
		t.Run(command, func(t *testing.T) {
			var output, stderr bytes.Buffer
			if err := Run(t.Context(), []string{command, "--config", missing}, &output, &stderr); err == nil {
				t.Fatal("missing config was accepted")
			}
			slog.Info("global logger configured", "command", command)
			logOutput, unused := &stderr, &output
			if command == "run" {
				logOutput, unused = &output, &stderr
			}
			if unused.Len() != 0 {
				t.Fatal("log written to the wrong stream", unused.String())
			}
			var record struct {
				Message string `json:"msg"`
				Command string `json:"command"`
			}
			if err := json.Unmarshal(logOutput.Bytes(), &record); err != nil {
				t.Fatal("default logger did not produce JSON", err, logOutput.String())
			}
			if record.Message != "global logger configured" || record.Command != command {
				t.Fatal("default logger lost structured output", record)
			}
		})
	}
}

func TestDefaultLoggerUsesSharedRedactor(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	var output, stderr bytes.Buffer
	missing := filepath.Join(testDirectory(t), "missing.yaml")
	if err := Run(t.Context(), []string{"status", "--config", missing}, &output, &stderr); err == nil {
		t.Fatal("missing config was accepted")
	}
	const secret = "cli-late-registered-secret"
	logreduct.Default.Add(secret)
	defer logreduct.Default.Release([]string{secret})
	slog.Info("credential", "token", secret)
	var record struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatal("default logger did not produce JSON", err, stderr.String())
	}
	if record.Token != "***" || strings.Contains(stderr.String(), secret) {
		t.Fatal("default logger did not use the shared redactor", stderr.String())
	}
}
