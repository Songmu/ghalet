package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet/fshelper"
)

func managedFixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	return root, filepath.Join(root, "config.yaml")
}

func TestManagedPaths(t *testing.T) {
	root, custom := managedFixture(t)
	standard, _ := ResolvePath("")
	first, err := ManagedPath(standard, "")
	if err != nil || first != filepath.Join(root, "state", "ghalet", "managed.yaml") {
		t.Fatal(first, err)
	}
	system, err := ManagedPath(SystemConfigPath, "")
	systemRoot, resolveErr := fshelper.ResolveDirectory("/var/lib/ghalet")
	if err != nil || resolveErr != nil || system != filepath.Join(systemRoot, "managed.yaml") {
		t.Fatal(system, err)
	}
	customPath, err := ManagedPath(custom, "")
	if err != nil || filepath.Dir(customPath) != filepath.Join(root, "state", "ghalet", "managed") {
		t.Fatal(customPath, err)
	}
	other, _ := ManagedPath(filepath.Join(root, "other.yaml"), "")
	cleaned, _ := ManagedPath(filepath.Join(root, "sub", "..", "config.yaml"), "")
	if customPath == other || customPath != cleaned {
		t.Fatal("configuration identity was not isolated and normalized")
	}
	selected, err := ManagedPath(custom, "./local/managed.yaml")
	if err != nil || selected != filepath.Join(root, "local", "managed.yaml") {
		t.Fatal(selected, err)
	}
	for _, selected := range []string{"config.yaml", "config.yaml.lock"} {
		if _, err := ManagedPath(custom, selected); err == nil {
			t.Fatal("managed file overlaps static configuration", selected)
		}
	}
	t.Setenv("STATE_DIRECTORY", filepath.Join(root, "service"))
	system, err = ManagedPath(SystemConfigPath, "")
	if err != nil || system != filepath.Join(root, "service", "managed.yaml") {
		t.Fatal(system, err)
	}
}

func TestManagedCredentialPrecedence(t *testing.T) {
	_, path := managedFixture(t)
	managedPath := filepath.Join(filepath.Dir(path), "generated", "managed.yaml")
	if err := os.Mkdir(filepath.Dir(managedPath), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("version: 1\ndefaults:\n  credential:\n    type: pat\n    tokenFile: managed.pat\ntargets:\n  - id: build\n    scope: https://github.com/owner/repo\n    credential:\n      type: github-app\n      clientId: generated\n      privateKeyFile: app.pem\n    installationId: 123\n  - id: removed\n    scope: https://github.com/owner/removed\n    credential: {}\n")
	if err := os.WriteFile(managedPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, defaults, target string
		want                   string
		installation           int64
	}{
		{name: "managed target", want: "github-app", installation: 123},
		{name: "managed target before static default", defaults: "defaults:\n  credential:\n    type: pat\n    tokenEnv: STATIC_PAT\n", want: "github-app", installation: 123},
		{name: "explicit gh", target: "    credential: {}\n", want: "gh"},
		{name: "explicit PAT", target: "    credential:\n      type: pat\n      tokenEnv: STATIC_PAT\n", want: "pat"},
		{name: "explicit App", target: "    credential:\n      type: github-app\n      clientId: static\n      privateKeyFile: static.pem\n    installationId: 456\n", want: "github-app", installation: 456},
	} {
		t.Run(tc.name, func(t *testing.T) {
			static := []byte("managedFile: generated/managed.yaml\n" + tc.defaults + "targets:\n  - id: build\n    scope: OWNER/REPO/\n" + tc.target + "  - id: fallback\n    scope: owner/other\n")
			config, err := ParseConfig(path, static)
			if err != nil {
				t.Fatal(err)
			}
			target := config.Targets[0]
			if len(config.Targets) != 2 || target.Credential.Type != tc.want || target.InstallationID != tc.installation {
				t.Fatal("managed merge changed static targets or combined authentication", config)
			}
			if tc.target == "" && target.Credential.PrivateKeyFile != filepath.Join(filepath.Dir(managedPath), "app.pem") {
				t.Fatal("managed credential path did not use the managed file's directory")
			}
			if tc.name == "explicit App" && target.Credential.PrivateKeyFile != filepath.Join(filepath.Dir(path), "static.pem") {
				t.Fatal("static credential path did not use the static config directory")
			}
			fallback := config.Targets[1].Credential
			if tc.defaults == "" && fallback.TokenFile != filepath.Join(filepath.Dir(managedPath), "managed.pat") {
				t.Fatal("managed default not inherited", fallback)
			}
			if tc.defaults != "" && fallback.TokenEnv != "STATIC_PAT" {
				t.Fatal("managed default replaced static default", fallback)
			}
		})
	}
}

func TestManagedScopeAndAuthenticationIsolation(t *testing.T) {
	_, path := managedFixture(t)
	managedPath := filepath.Join(filepath.Dir(path), "managed.yaml")
	managed := []byte("version: 1\ntargets:\n  - id: build\n    scope: https://github.com/owner/old\n    credential:\n      type: github-app\n      clientId: generated\n      privateKeyFile: app.pem\n    installationId: 123\n")
	if err := os.WriteFile(managedPath, managed, 0600); err != nil {
		t.Fatal(err)
	}
	static := "managedFile: managed.yaml\ntargets:\n  - id: build\n    scope: owner/new\n"
	if _, err := ParseConfig(path, []byte(static)); err == nil || !strings.Contains(err.Error(), "different scope") {
		t.Fatal("stale managed authentication applied to a new scope", err)
	}
	if _, err := ParseConfig(path, []byte(static+"    credential: {}\n")); err != nil {
		t.Fatal("explicit credential could not bypass unused managed binding", err)
	}
	if _, err := ParseConfig(path, []byte(static+"    installationId: 456\n")); err == nil {
		t.Fatal("manual installation ID was combined with managed target credentials")
	}
}

func TestManagedFilesRejectUnsafeOrInvalidData(t *testing.T) {
	root, path := managedFixture(t)
	managedPath := filepath.Join(root, "managed.yaml")
	for _, data := range []string{
		"", "version: 2\n", "version: 1\nunknown: value\n", "version: 1\n---\nversion: 1\n",
		"version: 1\ntargets:\n  - id: build\n    scope: https://github.com/owner/repo\n    credential:\n      type: github-app\n      clientId: app\n      privateKeyFile: key.pem\n",
	} {
		if _, err := ParseManaged([]byte(data)); err == nil {
			t.Fatal("invalid managed document accepted", data)
		}

	}
	for _, kind := range []string{"symlink", "directory", "writable", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dest := filepath.Join(root, kind+".yaml")
			switch kind {
			case "symlink":
				if err := os.Symlink(path, dest); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(dest, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				data, mode := []byte("version: 1\n"), os.FileMode(0666)
				if kind == "oversized" {
					data, mode = bytes.Repeat([]byte("a"), 1024*1024+1), 0600
				}
				if err := os.WriteFile(dest, data, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dest, mode); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := ReadManaged(dest); err == nil {
				t.Fatal("unsafe managed file accepted")
			}
		})
	}
	if err := WriteManaged(managedPath, nil, []byte("version: 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteManaged(managedPath, nil, []byte("version: 1\n")); err == nil {
		t.Fatal("concurrent managed file creation was overwritten")
	}
	if _, err := ParseConfig(path, []byte("managedFile: state/ghalet/logs/managed.yaml\ntargets: []\n")); err == nil {
		t.Fatal("managed file within cleaned log directory accepted")
	}
}

func TestManagedSizeLimitIncludesGeneratedHeader(t *testing.T) {
	root, _ := managedFixture(t)
	managed := &Managed{Version: 1, Defaults: DefaultsConfig{
		Credential: &Credential{Type: "pat", TokenFile: "x"},
	}}
	baseline, err := managed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	managed.Defaults.Credential.TokenFile = strings.Repeat("x", maxManagedBytes-len(baseline)+2)
	if _, err := managed.Marshal(); err == nil {
		t.Fatal("serialized managed document exceeded reader size limit")
	}
	path := filepath.Join(root, "managed.yaml")
	before := []byte("version: 1\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	oversized := append(bytes.Clone(before), []byte("#"+strings.Repeat("x", maxManagedBytes))...)
	if err := WriteManaged(path, before, oversized); err == nil {
		t.Fatal("oversized managed update accepted")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, before) {
		t.Fatal("oversized update changed existing managed document", err)
	}
}

func TestManagedPathRejectsFinalConfigSymlinkCollision(t *testing.T) {
	root, _ := managedFixture(t)
	actual := filepath.Join(root, "actual.yaml")
	if err := os.WriteFile(actual, []byte("targets: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "config.yaml")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedPath(link, actual); err == nil {
		t.Fatal("managed path aliases the final configuration symlink target")
	}
}
