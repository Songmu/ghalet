package ghalet

import (
	"bytes"
	"context"
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
targets:
  - id: build
    scope: https://github.com/owner/repository
    credential:
      type: pat
      tokenEnv: GHALET_TEST_TOKEN
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
	if c.Targets[0].Credential.TokenFile != filepath.Join(filepath.Dir(path), "credentials/token") {
		t.Fatal("credential path was not resolved relative to config")
	}
}

func TestConfigTargetScope(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"owner/repository", "https://github.com/owner/repository"},
		{"owner", "https://github.com/owner"},
		{"owner/repository/", "https://github.com/owner/repository"},
		{"owner/", "https://github.com/owner"},
		{"https://github.com/owner/repository/", "https://github.com/owner/repository"},
		{"https://ghe.example.com/owner/repository", "https://ghe.example.com/owner/repository"},
		{"https://ghe.example.com/owner", "https://ghe.example.com/owner"},
		{"", ""},
		{"owner//repository", ""},
		{"owner/repository/extra", ""},
		{"owner/../repository", ""},
		{"owner/repository?token=hidden", ""},
		{"owner/repository#fragment", ""},
		{"http:github.com/owner", ""},
		{"http://github.com/owner", ""},
		{"ssh://github.com/owner", ""},
		{"enterprises/company", ""},
		{"Enterprises/company", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			content := strings.Replace(testYAML, "https://github.com/owner/repository", tc.input, 1)
			c, err := LoadConfig(writeConfig(t, content))
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid registration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Targets[0].URL != tc.want {
				t.Fatalf("got %q, want %q", c.Targets[0].URL, tc.want)
			}
		})
	}
}

func TestConfigRejectsDuplicateShorthandRegistration(t *testing.T) {
	const extra = `
  - id: other
    scope: https://github.com/REGISTRATION
    credential:
      type: gh
    scaleSetName: build
    scaling:
      maxRunners: 2
runner:`
	for _, registration := range []string{"owner", "owner/repository"} {
		t.Run(registration, func(t *testing.T) {
			content := strings.Replace(testYAML, "https://github.com/owner/repository", registration, 1)
			content = strings.Replace(content, "\nrunner:", strings.Replace(extra, "REGISTRATION", registration, 1), 1)
			if _, err := LoadConfig(writeConfig(t, content)); err == nil ||
				!strings.Contains(err.Error(), "duplicate scope/group/scale set registration") {
				t.Fatal("shorthand and full URL bypassed duplicate registration validation", err)
			}
		})
	}
}

func TestConfigAliases(t *testing.T) {
	const input = `apiVersion: ghalet/v1alpha1
targets:
  - id: first
    scope: &scope first/
    credential: &credential
CREDENTIAL
FIRST_INSTALLATION
    scaleSetName: build
    scaling: &scaling
      minRunners: 0
      maxRunners: 2
  - id: second
    scope: *scope
    credential: *credential
SECOND_INSTALLATION
    scaleSetName: test
    scaling: *scaling
runner:
  version: "2.338.0"
`
	for _, tc := range []struct {
		name       string
		credential string
		app        bool
	}{
		{"gh", "      type: gh", false},
		{"PAT environment", "      type: pat\n      tokenEnv: SHARED_TOKEN", false},
		{"PAT file", "      type: pat\n      tokenFile: credentials/token", false},
		{"App key", "      type: github-app\n      clientId: app-client\n      privateKeyFile: credentials/app.pem", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstInstallation, secondInstallation := "", ""
			content := input
			if tc.app {
				firstInstallation, secondInstallation = "    installationId: 100", "    installationId: 200"
				content = strings.Replace(content, "scope: *scope", "scope: https://github.com/second", 1)
			}
			content = strings.NewReplacer("CREDENTIAL", tc.credential,
				"FIRST_INSTALLATION", firstInstallation, "SECOND_INSTALLATION", secondInstallation).Replace(content)
			path := writeConfig(t, content)
			c, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			first, second := c.Targets[0], c.Targets[1]
			if first.Credential != second.Credential || first.Scaling != second.Scaling ||
				first.URL != "https://github.com/first" || (!tc.app && first.URL != second.URL) {
				t.Fatal("aliases did not expand into target values", c.Targets)
			}
			if first.Credential.TokenFile != "" && first.Credential.TokenFile != filepath.Join(filepath.Dir(path), "credentials/token") {
				t.Fatal("aliased token path was not resolved relative to config")
			}
			if tc.app && (first.InstallationID != 100 || second.InstallationID != 200 ||
				first.Credential.PrivateKeyFile != filepath.Join(filepath.Dir(path), "credentials/app.pem")) {
				t.Fatal("shared App credential lost per-target installation IDs or relative path")
			}
			c.Targets[0].Credential.Type = "changed"
			if c.Targets[1].Credential.Type == "changed" {
				t.Fatal("alias caused targets to share mutable credential values")
			}
		})
	}
}

func TestConfigTargetCredentials(t *testing.T) {
	for _, tc := range []struct {
		name           string
		credential     Credential
		installationID int64
		valid          bool
	}{
		{"gh", Credential{Type: "gh"}, 0, true},
		{"gh host", Credential{Type: "gh", Hostname: "github.com"}, 0, true},
		{"wrong gh host", Credential{Type: "gh", Hostname: "ghe.example.com"}, 0, false},
		{"gh with PAT", Credential{Type: "gh", TokenEnv: "TOKEN"}, 0, false},
		{"PAT file", Credential{Type: "pat", TokenFile: "/private/token"}, 0, true},
		{"PAT invalid env", Credential{Type: "pat", TokenEnv: "BAD-NAME"}, 0, false},
		{"PAT host", Credential{Type: "pat", TokenEnv: "TOKEN", Hostname: "github.com"}, 0, false},
		{"App", Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, 100, true},
		{"App missing installation", Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, 0, false},
		{"App negative installation", Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, -1, false},
		{"App missing key", Credential{Type: "github-app", ClientID: "app"}, 100, false},
		{"App with PAT", Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem", TokenEnv: "TOKEN"}, 100, false},
		{"unknown type", Credential{Type: "other"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Targets[0].Credential, c.Targets[0].InstallationID = tc.credential, tc.installationID
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestConfigRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"unknown key":           testYAML + "unknown: true\n",
		"nested unknown key":    strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      unknown: true", 1),
		"duplicate YAML key":    testYAML + "apiVersion: ghalet/v1alpha1\n",
		"nested duplicate key":  strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      minRunners: 1", 1),
		"multiple documents":    testYAML + "---\napiVersion: ghalet/v1alpha1\n",
		"empty extra document":  testYAML + "---\n",
		"wrong version":         strings.Replace(testYAML, "ghalet/v1alpha1", "ghalet/v2", 1),
		"unfixed runner":        strings.Replace(testYAML, `"2.338.0"`, "latest", 1),
		"missing credential":    strings.Replace(testYAML, "    credential:\n      type: pat\n      tokenEnv: GHALET_TEST_TOKEN\n", "", 1),
		"missing type":          strings.Replace(testYAML, "      type: pat\n", "", 1),
		"missing scope":         strings.Replace(testYAML, "    scope: https://github.com/owner/repository\n", "", 1),
		"old URL key":           strings.Replace(testYAML, "scope: https://github.com/owner/repository", "url: https://github.com/owner/repository", 1),
		"legacy credentials":    testYAML + "credentials: {}\n",
		"legacy scopes":         testYAML + "scopes: {}\n",
		"legacy scopeRef":       strings.Replace(testYAML, "id: build", "id: build\n    scopeRef: repository", 1),
		"legacy credentialRef":  strings.Replace(testYAML, "id: build", "id: build\n    credentialRef: personal", 1),
		"repository group":      strings.Replace(testYAML, "scaleSetName: build", "runnerGroup: special\n    scaleSetName: build", 1),
		"enterprise":            strings.Replace(testYAML, "owner/repository", "enterprises/company", 1),
		"HTTP scope":            strings.Replace(testYAML, "https:", "http:", 1),
		"userinfo":              strings.Replace(testYAML, "github.com/", "user:secret@github.com/", 1),
		"query":                 strings.Replace(testYAML, "owner/repository", "owner/repository?token=abc", 1),
		"invalid target id":     strings.Replace(testYAML, "id: build", "id: ../outside", 1),
		"negative min":          strings.Replace(testYAML, "minRunners: 0", "minRunners: -1", 1),
		"min exceeds max":       strings.Replace(testYAML, "minRunners: 0", "minRunners: 3", 1),
		"missing max":           strings.Replace(testYAML, "maxRunners: 2", "maxRunners: 0", 1),
		"min exceeds global":    strings.Replace(testYAML, "minRunners: 0", "minRunners: 2", 1) + "capacity:\n  maxRunners: 1\n",
		"zero startup timeout":  testYAML + "cleanup:\n  logRetention: 0s\n",
		"two PAT inputs":        strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "tokenEnv: GHALET_TEST_TOKEN\n      tokenFile: token", 1),
		"inline token":          strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "token: secret", 1),
		"App installation":      strings.Replace(testYAML, "id: build", "id: build\n    installationId: 10", 1),
		"credential env":        strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    GH_TOKEN: never-copy", 1),
		"custom credential env": strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    GHALET_TEST_TOKEN: never-copy", 1),
		"identity env":          strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    HOME: /tmp", 1),
		"overlapping roots":     strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  cacheRoot: runs/downloads", 1),
		"root directory":        strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  workRoot: /", 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, input)); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestConfigRejectsRunnerInputEnvironmentCaseInsensitively(t *testing.T) {
	for _, name := range []string{
		"ACTIONS_RUNNER_INPUT_HELP",
		"actions_runner_input_help",
		"Actions_Runner_Input_Help",
		"actions_runner_input_jitconfig",
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    "+name+": \"true\"", 1)
			if _, err := LoadConfig(writeConfig(t, input)); err == nil {
				t.Fatal("runner input override was accepted", name)
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
	for _, input := range []string{"0s", "-1d", "invalid", "2562048d", "2562048h", "true", "[7d]", "{days: 7}", "1h1d", "2m1d", "1d2h"} {
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

func TestConfigurationRejectsLegacyScopeFlag(t *testing.T) {
	var output bytes.Buffer
	err := Run(t.Context(), []string{"destroy", "--scope", "repository"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -scope") {
		t.Fatal("legacy --scope flag was not rejected", err)
	}
}

func TestVersionFlagPlacementDoesNotStartDaemon(t *testing.T) {
	path := writeConfig(t, testYAML)
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
			if output.String() != cmdName+" v"+version+" (rev:"+revision+")\n" || stderr.Len() != 0 {
				t.Fatal("version query produced unexpected output", output.String(), stderr.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "runs")); !os.IsNotExist(err) {
		t.Fatal("version query touched daemon execution directories", err)
	}
}

func TestScopeURLValidation(t *testing.T) {
	for _, raw := range []string{"https://github.com/org", "https://github.com/org/repo", "https://ghe.example.com/org/repo"} {
		if _, err := scopeURL(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://github.com", "https://github.com/a/../b", "https://github.com/a/b/c", "https://github.com/a%2fb", "https://github.com:443/org", "https://github.com/Enterprises/acme", "https://github.com/ENTERPRISES/acme"} {
		if _, err := scopeURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
