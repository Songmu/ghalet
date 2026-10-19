package ghalet

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

const testYAML = `apiVersion: ghalet/v1alpha1
credentials:
  personal:
    type: pat
    tokenEnv: GHALET_TEST_TOKEN
scopes:
  repository:
    url: https://github.com/owner/repository
    credentialRef: personal
targets:
  - id: build
    scopeRef: repository
    scaleSetName: build
    scaling:
      minRunners: 0
      maxRunners: 2
runner:
  version: "2.338.0"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(testDirectory(t), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	c, err := LoadConfig(writeConfig(t, testYAML))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigDefaultsAndRelativePaths(t *testing.T) {
	c := testConfig(t)
	if c.Capacity.MaxRunners != 2 || c.Runner.StartupTimeout != Duration(2*time.Minute) ||
		c.Runner.DrainTimeout != Duration(10*time.Minute) ||
		!c.Cleanup.PreserveLogs || c.Cleanup.LogRetention != Duration(7*24*time.Hour) {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Runner.WorkRoot != filepath.Join(filepath.Dir(c.Path), "runs") {
		t.Fatal("workRoot must be relative to config, not the working directory")
	}
	path := writeConfig(t, strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "tokenFile: credentials/token", 1))
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Credentials["personal"].TokenFile != filepath.Join(filepath.Dir(path), "credentials/token") {
		t.Fatal("credential path was not resolved relative to config")
	}
}

func TestConfigRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":          testYAML + "unknown: true\n",
		"nested unknown key":   strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      unknown: true", 1),
		"duplicate YAML key":   testYAML + "apiVersion: ghalet/v1alpha1\n",
		"nested duplicate key": strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      minRunners: 1", 1),
		"multiple documents":   testYAML + "---\napiVersion: ghalet/v1alpha1\n",
		"empty extra document": testYAML + "---\n",
		"wrong version":        strings.Replace(testYAML, "ghalet/v1alpha1", "ghalet/v2", 1),
		"unfixed runner":       strings.Replace(testYAML, `"2.338.0"`, "latest", 1),
		"missing credential":   strings.Replace(testYAML, "credentialRef: personal", "credentialRef: missing", 1),
		"missing scope":        strings.Replace(testYAML, "scopeRef: repository", "scopeRef: missing", 1),
		"repository group":     strings.Replace(testYAML, "scaleSetName: build", "runnerGroup: special\n    scaleSetName: build", 1),
		"enterprise":           strings.Replace(testYAML, "owner/repository", "enterprises/company", 1),
		"HTTP scope":           strings.Replace(testYAML, "https:", "http:", 1),
		"userinfo":             strings.Replace(testYAML, "github.com/", "user:secret@github.com/", 1),
		"query":                strings.Replace(testYAML, "owner/repository", "owner/repository?token=abc", 1),
		"invalid target id":    strings.Replace(testYAML, "id: build", "id: ../outside", 1),
		"negative min":         strings.Replace(testYAML, "minRunners: 0", "minRunners: -1", 1),
		"min exceeds max":      strings.Replace(testYAML, "minRunners: 0", "minRunners: 3", 1),
		"missing max":          strings.Replace(testYAML, "maxRunners: 2", "maxRunners: 0", 1),
		"min exceeds global":   strings.Replace(testYAML, "minRunners: 0", "minRunners: 2", 1) + "capacity:\n  maxRunners: 1\n",
		"zero startup timeout": testYAML + "cleanup:\n  logRetention: 0s\n",
		"two PAT inputs":       strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "tokenEnv: GHALET_TEST_TOKEN\n    tokenFile: token", 1),
		"inline token":         strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "token: secret", 1),
		"App installation":     strings.Replace(testYAML, "credentialRef: personal", "credentialRef: personal\n    installationId: 10", 1),
		"credential env":       strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    GH_TOKEN: never-copy", 1),
		"identity env":         strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    HOME: /tmp", 1),
		"overlapping roots":    strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  cacheRoot: runs/downloads", 1),
		"root directory":       strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  workRoot: /", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, input)); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestConfigDurationDecoding(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{`"1.5d"`, 36 * time.Hour},
		{"'90m'", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
	} {
		t.Run(tc.input, func(t *testing.T) {
			c, err := LoadConfig(writeConfig(t, testYAML+"cleanup:\n  logRetention: "+tc.input+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Cleanup.LogRetention != Duration(tc.want) {
				t.Fatalf("got %v, want %v", time.Duration(c.Cleanup.LogRetention), tc.want)
			}
		})
	}
	for _, input := range []string{"0s", "-1d", "invalid", "2562048d", "2562048h", "true", "[7d]", "{days: 7}"} {
		t.Run(input, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, testYAML+"cleanup:\n  logRetention: "+input+"\n")); err == nil {
				t.Fatal("invalid duration was accepted")
			}
		})
	}
}

func TestConfigDuplicateRegistrationAndID(t *testing.T) {
	for _, duplicateID := range []bool{false, true} {
		c := testConfig(t)
		target := c.Targets[0]
		if !duplicateID {
			target.ID = "other"
		}
		c.Targets = append(c.Targets, target)
		if err := c.Validate(); err == nil {
			t.Fatal("duplicate accepted")
		}
	}
}

func TestConfigurationValidationCLI(t *testing.T) {
	path := writeConfig(t, testYAML)
	for _, args := range [][]string{
		{"config", "validate", "--config", path},
		{"--config", path, "config", "validate"},
		{"--config", path, "config", "validate", "--config", path},
	} {
		var output, stderr bytes.Buffer
		if err := Run(t.Context(), args, &output, &stderr); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, stderr.String())
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

func TestScopeURLValidation(t *testing.T) {
	for _, raw := range []string{"https://github.com/org", "https://github.com/org/repo", "https://ghe.example.com/org/repo"} {
		if _, err := scopeURL(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://github.com", "https://github.com/a/../b", "https://github.com/a/b/c", "https://github.com/a%2fb", "https://github.com:443/org"} {
		if _, err := scopeURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
