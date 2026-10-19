package cmd

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
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
	for _, command := range []string{"run", "status", "doctor", "drain", "gc", "destroy"} {
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

func TestVersionFlagPlacementDoesNotStartDaemon(t *testing.T) {
	path := writeConfig(t)
	missing := filepath.Join(filepath.Dir(path), "missing.yaml")
	t.Setenv("GHALET_TEST_TOKEN", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{
		{"--version"},
		{"run", "--version"},
		{"run", "--config", path, "--version"},
		{"--config", path, "run", "--version"},
		{"--config", path, "run", "--target", "build", "--version"},
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
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "runs")); !os.IsNotExist(err) {
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
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output, stderr bytes.Buffer
			err := Run(t.Context(), tc.args, &output, &stderr)
			if tc.wantHelp {
				if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "Usage: ghalet") {
					t.Fatalf("expected help, got %v (%s)", err, stderr.String())
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
	for _, name := range []string{"run", "validate", "status", "doctor", "drain", "gc", "destroy"} {
		t.Run(name, func(t *testing.T) {
			original := commands[name]
			t.Cleanup(func() { commands[name] = original })
			want := options{
				config: "after.yaml",
				control: ghalet.ControlOptions{
					Target: "build", DryRun: true, RunnerGroup: "builders", ScaleSet: "runners",
				},
			}
			ctx := t.Context()
			var output, stderr bytes.Buffer
			called := false
			wantErr := errors.New("command result")
			commands[name] = func(gotCtx context.Context, got options, outStream, errStream io.Writer) error {
				called = true
				if gotCtx != ctx || got != want || outStream != &output || errStream != &stderr {
					t.Fatalf("command inputs were not forwarded: %+v", got)
				}
				return wantErr
			}
			command := []string{name}
			if name == "validate" {
				command = []string{"config", "validate"}
			}
			for _, prefix := range [][]string{nil, {"--config", "before.yaml", "--target", "before"}} {
				args := append(append([]string{}, prefix...), command...)
				args = append(args, "--config", "after.yaml", "--target", "build", "--dry-run", "--runner-group", "builders", "--scale-set", "runners")
				called = false
				if err := Run(ctx, args, &output, &stderr); !errors.Is(err, wantErr) || !called {
					t.Fatalf("%v: command was not dispatched: %v", args, err)
				}
			}
		})
	}
}
