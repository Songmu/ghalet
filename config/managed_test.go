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
	data := []byte("defaults:\n  credential:\n    type: pat\n    tokenFile: managed.pat\ntargets:\n  - scaleSet: build\n    scope: https://github.com/owner/repo\n    credential:\n      type: github-app\n      clientId: generated\n      privateKeyFile: app.pem\n    installationId: 123\n  - scaleSet: removed\n    scope: https://github.com/owner/removed\n    credential: {}\n")
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
			static := []byte("managedFile: generated/managed.yaml\n" + tc.defaults + "targets:\n  - scaleSet: build\n    scope: OWNER/REPO/\n" + tc.target + "  - scaleSet: fallback\n    scope: owner/other\n")
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
	managed := []byte("targets:\n  - scaleSet: build\n    scope: https://github.com/owner/old\n    credential:\n      type: github-app\n      clientId: generated\n      privateKeyFile: app.pem\n    installationId: 123\n")
	if err := os.WriteFile(managedPath, managed, 0600); err != nil {
		t.Fatal(err)
	}
	static := "managedFile: managed.yaml\ntargets:\n  - scaleSet: build\n    scope: owner/new\n"
	config, err := ParseConfig(path, []byte(static))
	if err != nil || config.Targets[0].Credential.Type != "gh" {
		t.Fatal("stale managed authentication applied to a new registration", err)
	}
	if _, err := ParseConfig(path, []byte(static+"    installationId: 456\n")); err == nil {
		t.Fatal("manual installation ID was combined with managed target credentials")
	}
}

func TestManagedRegistrationMatching(t *testing.T) {
	_, path := managedFixture(t)
	managed := &Managed{}
	for i, target := range []ManagedTarget{
		{Scope: "owner", ScaleSetName: "build"},
		{Scope: "owner", RunnerGroup: "linux", ScaleSetName: "build"},
		{Scope: "owner", ScaleSetName: "test"},
	} {
		target.Credential = Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "app.pem"}
		target.InstallationID = int64(i + 1)
		managed.SetTarget(target)
	}
	replacement := managed.Targets[0]
	replacement.Scope = "https://GITHUB.COM/OWNER/"
	replacement.RunnerGroup = RunnerGroupName("")
	replacement.InstallationID = 42
	managed.SetTarget(replacement)
	if len(managed.Targets) != 3 {
		t.Fatal("equivalent registration was appended instead of replaced")
	}
	data, err := managed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "managed.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	static := "managedFile: managed.yaml\ntargets:\n  - scope: OWNER/\n    scaleSet: build\n  - scope: owner\n    scaleSet: build\n    runnerGroup: linux\n  - scope: owner\n    scaleSet: test\n  - scope: another\n    scaleSet: build\n"
	config, err := ParseConfig(path, []byte(static))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{42, 2, 3, 0} {
		if config.Targets[i].InstallationID != want {
			t.Fatal("managed authentication crossed registration boundaries", config.Targets)
		}
	}
	managed.Targets = append(managed.Targets, managed.Targets[0])
	if _, err := managed.Marshal(); err == nil {
		t.Fatal("duplicate managed registration accepted")
	}
}

func TestManagedFilesRejectUnsafeOrInvalidData(t *testing.T) {
	root, path := managedFixture(t)
	managedPath := filepath.Join(root, "managed.yaml")
	for _, data := range []string{
		"", "{}\n---\n{}\n",
		"targets: []\ntargets: []\n",
		"targets:\n  - scope: owner/repo\n    scope: owner/other\n    scaleSet: build\n    credential: {}\n",
		"targets:\n  - scaleSet: build\n    scope: https://github.com/owner/repo\n    credential:\n      type: github-app\n      clientId: app\n      privateKeyFile: key.pem\n",
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
				data, mode := []byte("{}\n"), os.FileMode(0666)
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
	if err := WriteManaged(managedPath, nil, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteManaged(managedPath, nil, []byte("{}\n")); err == nil {
		t.Fatal("concurrent managed file creation was overwritten")
	}
	if _, err := ParseConfig(path, []byte("managedFile: state/ghalet/logs/managed.yaml\ntargets: []\n")); err == nil {
		t.Fatal("managed file within cleaned log directory accepted")
	}
}

func TestManagedIgnoresUnknownProperties(t *testing.T) {
	data := []byte("version: 1\nunknown: [true, 42]\ndefaults:\n  unknown: value\n  credential:\n    unknown: value\ntargets:\n  - id: ignored\n    scope: owner/repo\n    scaleSet: build\n    credential:\n      unknown: value\n")
	managed, err := ParseManaged(data)
	if err != nil {
		t.Fatal(err)
	}
	if managed.Defaults.Credential == nil || managed.Defaults.Credential.Type != "gh" ||
		len(managed.Targets) != 1 || managed.Targets[0].Credential.Type != "gh" ||
		managed.Targets[0].Scope != "owner/repo" || managed.Targets[0].ScaleSetName != "build" {
		t.Fatalf("unknown properties changed managed configuration: %+v", managed)
	}
}

func TestManagedEmptyDocumentRoundTrip(t *testing.T) {
	managed := &Managed{}
	data, err := managed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("version:")) {
		t.Fatal("managed document contains a version field")
	}
	parsed, err := ParseManaged(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Defaults.Credential != nil || len(parsed.Targets) != 0 {
		t.Fatalf("empty managed document gained settings: %+v", parsed)
	}
}

func TestManagedSizeLimitIncludesGeneratedHeader(t *testing.T) {
	root, _ := managedFixture(t)
	managed := &Managed{Defaults: DefaultsConfig{
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
	before := []byte("{}\n")
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

func TestManagedFilesCannotContainRuntimeDirectories(t *testing.T) {
	root, path := managedFixture(t)
	base, err := ParseConfig(path, []byte("managedFile: generated\ntargets: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"logs", "work", "cache"} {
		for _, parent := range []string{"generated", "generated.lock"} {
			t.Run(field+"/"+parent, func(t *testing.T) {
				config := *base
				directory := filepath.Join(root, parent, "runtime")
				switch field {
				case "logs":
					config.Logs.Root = directory
				case "work":
					config.Runner.WorkRoot = directory
				case "cache":
					config.Runner.CacheRoot = directory
				}
				want := "managedFile and its lock must not overlap runner or log directories: " + config.ManagedFile
				if err := config.validateDirectories(); err == nil || err.Error() != want {
					t.Fatal("runtime directory below a managed file or lock was not rejected", err)
				}
			})
		}
	}
	if _, err := ParseConfig(path, []byte("managedFile: generated/managed.yaml\nlogs:\n  root: generated/logs\ntargets: []\n")); err != nil {
		t.Fatal("managed file and runtime directory cannot share a parent", err)
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

func TestCheckManagedPathsRejectsReservedOverlaps(t *testing.T) {
	root, static := managedFixture(t)
	managed := filepath.Join(root, "managed.yaml")
	for _, reserved := range []string{
		filepath.Join(managed, "key.pem"),
		filepath.Dir(managed),
		filepath.Join(managed+".lock", "key.pem"),
		filepath.Join(static, "key.pem"),
	} {
		t.Run(reserved, func(t *testing.T) {
			if err := CheckManagedPaths(managed, static, reserved); err == nil {
				t.Fatal("overlapping persistence paths accepted")
			}
		})
	}
}
