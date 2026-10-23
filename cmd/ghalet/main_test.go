package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerHelperCommands(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "ghalet")
	build := exec.Command("go", "build", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		input   string
		wantErr string
		json    bool
	}{
		{
			name: "user operation",
			args: []string{"__ghalet_runner_user"}, input: "{\"operation\":\"unknown\"}\n",
			wantErr: "runner user:", json: true,
		},
		{
			name:    "user launch mismatch",
			args:    []string{"__ghalet_runner_user", "/wrong/Runner.Listener"},
			input:   "{\"operation\":\"launch\",\"runnerDir\":\"/not-executed\"}\n",
			wantErr: "runner user:",
		},
		{
			name:    "guard shared group",
			args:    []string{"__ghalet_runner_guard", "/not-executed/Runner.Listener"},
			wantErr: "runner guard: refusing to guard a process group not led by this process",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(executable, tc.args...)
			command.Stdin = strings.NewReader(tc.input)
			var output, diagnostic bytes.Buffer
			command.Stdout, command.Stderr = &output, &diagnostic
			if err := command.Run(); err == nil || !strings.Contains(diagnostic.String(), tc.wantErr) {
				t.Fatalf("helper did not fail as expected: %v, stdout=%q, stderr=%q", err, &output, &diagnostic)
			}
			if tc.json {
				var result struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Error == "" {
					t.Fatalf("missing helper error result: %v, stdout=%q", err, &output)
				}
			} else if output.Len() != 0 {
				t.Fatalf("unexpected helper stdout: %q", &output)
			}
			if os.Geteuid() != 0 && tc.name == "user launch mismatch" &&
				!strings.Contains(diagnostic.String(), "runner command does not match") {
				t.Fatalf("launch did not validate its explicit runner argument: %q", &diagnostic)
			}
		})
	}
	if os.Geteuid() != 0 {
		t.Run("user success", func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			request, err := json.Marshal(map[string]string{
				"operation": "release", "runnerDir": directory, "workspace": directory,
			})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(executable, "__ghalet_runner_user")
			command.Stdin = bytes.NewReader(append(request, '\n'))
			var output, diagnostic bytes.Buffer
			command.Stdout, command.Stderr = &output, &diagnostic
			if err := command.Run(); err != nil || output.String() != "{}\n" || diagnostic.Len() != 0 {
				t.Fatalf("helper did not succeed: %v, stdout=%q, stderr=%q", err, &output, &diagnostic)
			}
		})
	}
}
