package ghalet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

func TestConfigCanonicalTrustedBases(t *testing.T) {
	for _, source := range []string{"service", "xdg", "home"} {
		t.Run(source, func(t *testing.T) {
			canonical := testDirectory(t)
			link := filepath.Join(testDirectory(t), "layout")
			if err := os.Symlink(canonical, link); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STATE_DIRECTORY", "")
			t.Setenv("CACHE_DIRECTORY", "")
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("XDG_CACHE_HOME", "")
			t.Setenv("HOME", link)
			stateBase := filepath.Join(canonical, ".local", "state", "ghalet")
			cacheBase := filepath.Join(canonical, ".cache", "ghalet")
			switch source {
			case "service":
				t.Setenv("STATE_DIRECTORY", filepath.Join(link, "state"))
				t.Setenv("CACHE_DIRECTORY", filepath.Join(link, "cache"))
				stateBase, cacheBase = filepath.Join(canonical, "state"), filepath.Join(canonical, "cache")
			case "xdg":
				t.Setenv("XDG_STATE_HOME", filepath.Join(link, "state"))
				t.Setenv("XDG_CACHE_HOME", filepath.Join(link, "cache"))
				stateBase, cacheBase = filepath.Join(canonical, "state", "ghalet"), filepath.Join(canonical, "cache", "ghalet")
			}
			path := filepath.Join(link, "config.yaml")
			c, err := settings.ParseConfig(path, []byte(readFixture(t, "config/base.yaml")))
			if err != nil {
				t.Fatal(err)
			}
			for got, want := range map[string]string{
				c.Runner.WorkRoot:  filepath.Join(stateBase, "runs"),
				c.Runner.CacheRoot: filepath.Join(cacheBase, "runners"),
				c.Logs.Root:        filepath.Join(stateBase, "logs"),
			} {
				if got != want {
					t.Fatalf("got %q, want canonical %q", got, want)
				}
			}
			if c.Path != path || socketPath(c.Path) != socketPath(path) ||
				socketPath(c.Path) == socketPath(filepath.Join(canonical, "config.yaml")) {
				t.Fatal("canonical storage changed lexical config/control identity")
			}
			if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0755); err != nil {
				t.Fatal("canonical execution path was rejected", err)
			}
			if _, err := os.Stat(c.Runner.CacheRoot); !os.IsNotExist(err) {
				t.Fatal("parsing created cache directories", err)
			}
		})
	}
}

func TestConfigCanonicalLogRootAndOverlap(t *testing.T) {
	c := testConfig(t)
	link := filepath.Join(testDirectory(t), "logs-layout")
	if err := os.Symlink(testDirectory(t), link); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	content := readFixture(t, "config/base.yaml") + "logs:\n  root: " + link + "\n"
	parsed, err := settings.ParseConfig(c.Path, []byte(content))
	if err != nil || parsed.Logs.Root != canonical {
		t.Fatal("explicit log base was not canonicalized", parsed, err)
	}
	if err := fshelper.EnsureDirectory(c.Runner.WorkRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.Runner.WorkRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.ParseConfig(c.Path, []byte(content)); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatal("symlink alias bypassed storage overlap validation", err)
	}
}

func TestConfigDoesNotResolveManagedStorageChildren(t *testing.T) {
	for _, child := range []string{"runs", "runners", "logs"} {
		t.Run(child, func(t *testing.T) {
			base := testDirectory(t)
			t.Setenv("STATE_DIRECTORY", base)
			t.Setenv("CACHE_DIRECTORY", base)
			path := filepath.Join(base, child)
			outside := testDirectory(t)
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			c, err := settings.ParseConfig(writeConfig(t, ""), []byte(readFixture(t, "config/base.yaml")))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{"runs": c.Runner.WorkRoot, "runners": c.Runner.CacheRoot, "logs": c.Logs.Root}[child]
			if got != path {
				t.Fatal("managed child was treated as a trusted base", got)
			}
			if err := fshelper.EnsureDirectory(got, 0700); err == nil {
				t.Fatal("symlinked managed child was accepted")
			}
		})
	}
}

func TestConfigStateDirectories(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	testYAML := readFixture(t, "config/base.yaml")
	home, stateHome := testDirectory(t), testDirectory(t)
	for _, tc := range []struct {
		name, stateHome, want string
		unset                 bool
	}{
		{"unset", "", filepath.Join(home, ".local", "state", "ghalet", "logs"), true},
		{"empty", "", filepath.Join(home, ".local", "state", "ghalet", "logs"), false},
		{"relative", "relative/state", filepath.Join(home, ".local", "state", "ghalet", "logs"), false},
		{"absolute", stateHome, filepath.Join(stateHome, "ghalet", "logs"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", tc.stateHome)
			if tc.unset {
				if err := os.Unsetenv("XDG_STATE_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			c, err := settings.LoadConfig(writeConfig(t, testYAML))
			if err != nil {
				t.Fatal(err)
			}
			if c.Logs.Root != tc.want {
				t.Fatalf("got %q, want %q", c.Logs.Root, tc.want)
			}
			wantWork := filepath.Join(filepath.Dir(tc.want), "runs")
			if c.Runner.WorkRoot != wantWork {
				t.Fatalf("got work root %q, want %q", c.Runner.WorkRoot, wantWork)
			}
			other, err := settings.LoadConfig(writeConfig(t, testYAML))
			if err != nil {
				t.Fatal(err)
			}
			if other.Runner.WorkRoot != c.Runner.WorkRoot {
				t.Fatal("work root must not depend on the config directory")
			}
			for _, path := range []string{c.Logs.Root, c.Runner.WorkRoot} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("loading config must not create %s: %v", path, err)
				}
			}
		})
	}
}

func TestConfigRunnerCacheDirectory(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("CACHE_DIRECTORY", "")
	home, cacheHome := testDirectory(t), testDirectory(t)
	for _, tc := range []struct {
		name, value, want string
		unset             bool
	}{
		{"unset", "", filepath.Join(home, ".cache", "ghalet", "runners"), true},
		{"empty", "", filepath.Join(home, ".cache", "ghalet", "runners"), false},
		{"relative", "relative/cache", filepath.Join(home, ".cache", "ghalet", "runners"), false},
		{"absolute", cacheHome, filepath.Join(cacheHome, "ghalet", "runners"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", testDirectory(t))
			t.Setenv("XDG_CACHE_HOME", tc.value)
			if tc.unset {
				if err := os.Unsetenv("XDG_CACHE_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			c, err := settings.LoadConfig(writeConfig(t, readFixture(t, "config/base.yaml")))
			if err != nil {
				t.Fatal(err)
			}
			if c.Runner.CacheRoot != tc.want {
				t.Fatalf("got %q, want %q", c.Runner.CacheRoot, tc.want)
			}
			if _, err := os.Stat(c.Runner.CacheRoot); !os.IsNotExist(err) {
				t.Fatalf("loading config must not create the runner cache: %v", err)
			}
		})
	}
}

func TestConfigServiceDirectories(t *testing.T) {
	stateDir, cacheDir := testDirectory(t), testDirectory(t)
	stateHome, cacheHome := testDirectory(t), testDirectory(t)
	for _, tc := range []struct {
		name, state, cache string
		unset              bool
	}{
		{"unset", "", "", true},
		{"empty", "", "", false},
		{"state only", stateDir, "", false},
		{"cache only", "", cacheDir, false},
		{"both", stateDir, cacheDir, false},
		{"shared base", stateDir, stateDir, false},
		{"normalized", stateDir + "/nested/..", cacheDir + "/nested/..", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STATE_DIRECTORY", tc.state)
			t.Setenv("CACHE_DIRECTORY", tc.cache)
			t.Setenv("XDG_STATE_HOME", stateHome)
			t.Setenv("XDG_CACHE_HOME", cacheHome)
			if tc.unset {
				for _, name := range []string{"STATE_DIRECTORY", "CACHE_DIRECTORY"} {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
			}
			c, err := settings.LoadConfig(writeConfig(t, readFixture(t, "config/base.yaml")))
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantCache := filepath.Join(stateHome, "ghalet"), filepath.Join(cacheHome, "ghalet")
			if tc.state != "" {
				wantState = filepath.Clean(tc.state)
			}
			if tc.cache != "" {
				wantCache = filepath.Clean(tc.cache)
			}
			for _, directory := range []struct {
				got, want string
			}{
				{c.Runner.WorkRoot, filepath.Join(wantState, "runs")},
				{c.Runner.CacheRoot, filepath.Join(wantCache, "runners")},
				{c.Logs.Root, filepath.Join(wantState, "logs")},
			} {
				if directory.got != directory.want {
					t.Fatalf("got %q, want %q", directory.got, directory.want)
				}
				if _, err := os.Stat(directory.got); !os.IsNotExist(err) {
					t.Fatalf("loading config must not create %s: %v", directory.got, err)
				}
			}
		})
	}
}

func TestConfigRejectsInvalidServiceDirectories(t *testing.T) {
	for _, name := range []string{"STATE_DIRECTORY", "CACHE_DIRECTORY"} {
		for _, value := range []string{"relative/path", "/first:/second", ":/second", "/first:"} {
			t.Run(name+"="+value, func(t *testing.T) {
				t.Setenv("STATE_DIRECTORY", "")
				t.Setenv("CACHE_DIRECTORY", "")
				t.Setenv("XDG_STATE_HOME", testDirectory(t))
				t.Setenv("XDG_CACHE_HOME", testDirectory(t))
				t.Setenv(name, value)
				if _, err := settings.LoadConfig(writeConfig(t, readFixture(t, "config/base.yaml"))); err == nil ||
					!strings.Contains(err.Error(), name+" must specify a single absolute directory") {
					t.Fatalf("expected invalid service directory to be rejected, got %v", err)
				}
			})
		}
	}
}

func TestConfigServiceLogRootOverride(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", testDirectory(t))
	t.Setenv("CACHE_DIRECTORY", testDirectory(t))
	path := writeConfig(t, readFixture(t, "config/base.yaml")+"logs:\n  root: archived-logs\n")
	c, err := settings.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "archived-logs"); c.Logs.Root != want {
		t.Fatalf("got %q, want %q", c.Logs.Root, want)
	}
}

func TestConfigRejectsOverlappingXDGDirectories(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("CACHE_DIRECTORY", "")
	for _, name := range []string{"logs in runs", "logs in cache", "cache in runs"} {
		t.Run(name, func(t *testing.T) {
			stateHome, cacheHome := testDirectory(t), testDirectory(t)
			t.Setenv("XDG_STATE_HOME", stateHome)
			t.Setenv("XDG_CACHE_HOME", cacheHome)
			content := readFixture(t, "config/base.yaml")
			switch name {
			case "logs in runs":
				content += "logs:\n  root: " + filepath.Join(stateHome, "ghalet", "runs", "logs") + "\n"
			case "logs in cache":
				content += "logs:\n  root: " + filepath.Join(cacheHome, "ghalet", "runners") + "\n"
			case "cache in runs":
				t.Setenv("XDG_CACHE_HOME", filepath.Join(stateHome, "ghalet", "runs"))
			}
			if _, err := settings.LoadConfig(writeConfig(t, content)); err == nil || !strings.Contains(err.Error(), "must not overlap") {
				t.Fatalf("expected overlapping directories to be rejected, got %v", err)
			}
		})
	}
}

func TestConfigRejectsOverlappingServiceDirectories(t *testing.T) {
	for _, name := range []string{"logs in runs", "cache in runs", "logs in cache"} {
		t.Run(name, func(t *testing.T) {
			stateDir, cacheDir := testDirectory(t), testDirectory(t)
			content := readFixture(t, "config/base.yaml")
			switch name {
			case "logs in runs":
				content += "logs:\n  root: " + filepath.Join(stateDir, "runs") + "\n"
			case "cache in runs":
				cacheDir = filepath.Join(stateDir, "runs")
			case "logs in cache":
				content += "logs:\n  root: " + filepath.Join(cacheDir, "runners") + "\n"
			}
			t.Setenv("STATE_DIRECTORY", stateDir)
			t.Setenv("CACHE_DIRECTORY", cacheDir)
			if _, err := settings.LoadConfig(writeConfig(t, content)); err == nil || !strings.Contains(err.Error(), "must not overlap") {
				t.Fatalf("expected overlapping directories to be rejected, got %v", err)
			}
		})
	}
}

func TestConfigLogRootOverride(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	testYAML := readFixture(t, "config/base.yaml")
	t.Setenv("XDG_STATE_HOME", testDirectory(t))
	for _, root := range []string{"logs", "custom/logs", filepath.Join(testDirectory(t), "logs")} {
		t.Run(root, func(t *testing.T) {
			path := writeConfig(t, testYAML+"logs:\n  root: "+root+"\n")
			c, err := settings.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			want := root
			if !filepath.IsAbs(want) {
				want = filepath.Join(filepath.Dir(path), want)
			}
			if c.Logs.Root != want {
				t.Fatalf("got %q, want %q", c.Logs.Root, want)
			}
		})
	}
}

func TestConfigLogs(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	for _, tc := range []struct {
		name, logs string
		preserve   bool
		retention  time.Duration
	}{
		{"omitted", "", true, 7 * 24 * time.Hour},
		{"empty mapping", "logs: {}\n", true, 7 * 24 * time.Hour},
		{"preservation disabled", "logs:\n  preserve: false\n", false, 7 * 24 * time.Hour},
		{"custom retention", "logs:\n  retention: 1.5d\n", true, 36 * time.Hour},
		{"all fields", "logs:\n  preserve: false\n  retention: 2d\n  root: archived-logs\n", false, 48 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, testYAML+tc.logs)
			c, err := settings.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Logs.Preserve != tc.preserve || c.Logs.Retention != settings.Duration(tc.retention) {
				t.Fatalf("unexpected log settings: %+v", c.Logs)
			}
			if tc.name == "all fields" && c.Logs.Root != filepath.Join(filepath.Dir(path), "archived-logs") {
				t.Fatalf("unexpected log root: %q", c.Logs.Root)
			}
		})
	}
}

func TestConfigLogRootWithoutHome(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("CACHE_DIRECTORY", "")
	testYAML := readFixture(t, "config/base.yaml")
	t.Setenv("XDG_CACHE_HOME", testDirectory(t))
	t.Setenv("HOME", "")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	for _, env := range []struct {
		name, value string
		unset       bool
	}{
		{"unset", "", true},
		{"empty", "", false},
		{"relative", "relative/state", false},
		{"absolute", testDirectory(t), false},
	} {
		t.Run(env.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", env.value)
			if env.unset {
				if err := os.Unsetenv("XDG_STATE_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				name, logs, wantErr string
				root                string
			}{
				{name: "omitted", wantErr: "resolve state directory"},
				{name: "empty mapping", logs: "logs: {}\n", wantErr: "resolve state directory"},
				{name: "empty path", logs: "logs:\n  root: \"\"\n", wantErr: "empty"},
				{name: "relative override", root: "custom/logs"},
				{name: "absolute override", root: filepath.Join(testDirectory(t), "logs")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					content := testYAML + tc.logs
					if tc.root != "" {
						content += "logs:\n  root: " + tc.root + "\n"
					}
					path := writeConfig(t, content)
					c, err := settings.LoadConfig(path)
					wantErr := tc.wantErr
					if !filepath.IsAbs(env.value) {
						wantErr = "resolve state directory"
					} else if tc.name != "empty path" {
						wantErr = ""
					}
					if wantErr != "" {
						if err == nil {
							t.Fatal("config without a usable log directory was accepted")
						}
						if tc.name != "empty path" && !strings.Contains(err.Error(), wantErr) {
							t.Fatalf("expected %q, got %v", wantErr, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					want := tc.root
					if want == "" {
						want = filepath.Join(env.value, "ghalet", "logs")
					} else if !filepath.IsAbs(want) {
						want = filepath.Join(filepath.Dir(path), want)
					}
					if c.Logs.Root != want {
						t.Fatalf("got %q, want %q", c.Logs.Root, want)
					}
				})
			}
		})
	}
}

func TestConfigDefaultsAndRelativePaths(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	c := testConfig(t)
	if c.Capacity.MaxRunners != 2 || c.Runner.StartupTimeout != settings.Duration(2*time.Minute) ||
		c.Runner.DrainTimeout != settings.Duration(10*time.Minute) ||
		!c.Logs.Preserve || c.Logs.Retention != settings.Duration(7*24*time.Hour) {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Runner.WorkRoot != filepath.Join(os.Getenv("XDG_STATE_HOME"), "ghalet", "runs") {
		t.Fatal("work root must be under XDG_STATE_HOME")
	}
	path := writeConfig(t, strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "tokenFile: credentials/token", 1))
	c, err := settings.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets[0].Credential.TokenFile != filepath.Join(filepath.Dir(path), "credentials/token") {
		t.Fatal("credential path was not resolved relative to config")
	}
}

func TestConfigCredentialDefaults(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	const credential = "    credential:\n      type: pat\n      tokenEnv: GHALET_TEST_TOKEN\n"
	for _, tc := range []struct {
		name, input string
		want        settings.Credential
	}{
		{"omitted", "", settings.Credential{Type: "gh"}},
		{"empty", "    credential: {}\n", settings.Credential{Type: "gh"}},
		{"hostname only", "    credential:\n      hostname: github.com\n", settings.Credential{Type: "gh", Hostname: "github.com"}},
		{"explicit gh", "    credential:\n      type: gh\n", settings.Credential{Type: "gh"}},
		{"explicit PAT", credential, settings.Credential{Type: "pat", TokenEnv: "GHALET_TEST_TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(testYAML, credential, tc.input, 1)
			c, err := settings.LoadConfig(writeConfig(t, content))
			if err != nil {
				t.Fatal(err)
			}
			if c.Targets[0].Credential != tc.want {
				t.Fatalf("got %+v, want %+v", c.Targets[0].Credential, tc.want)
			}
		})
	}
}

func TestConfigMinimal(t *testing.T) {
	input := readFixture(t, "config/minimal.yaml")
	for _, tc := range []struct {
		name, prefix string
	}{
		{"without schema comment", ""},
		{"with schema comment", "# yaml-language-server: $schema=https://raw.githubusercontent.com/Songmu/ghalet/main/schema.yaml\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := settings.LoadConfig(writeConfig(t, tc.prefix+input))
			if err != nil {
				t.Fatal(err)
			}
			target := c.Targets[0]
			if target.Credential != (settings.Credential{Type: "gh"}) ||
				target.ScaleSetName != "build" || target.Scaling != (settings.ScalingConfig{MaxRunners: 2}) ||
				c.Runner.Version != settings.DefaultRunnerVersion {
				t.Fatalf("unexpected minimal config defaults: %+v", c)
			}
		})
	}
}

func TestConfigRunnerVersionDefault(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	for _, tc := range []struct {
		name, runner, want string
	}{
		{"omitted runner", "", settings.DefaultRunnerVersion},
		{"empty runner", "runner: {}\n", settings.DefaultRunnerVersion},
		{"omitted version", "runner:\n  startupTimeout: 3m\n", settings.DefaultRunnerVersion},
		{"explicit version", "runner:\n  version: \"2.337.0\"\n", "2.337.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(testYAML, "runner:\n  version: \"2.338.0\"\n", tc.runner, 1)
			c, err := settings.LoadConfig(writeConfig(t, content))
			if err != nil {
				t.Fatal(err)
			}
			if c.Runner.Version != tc.want {
				t.Fatalf("got %q, want %q", c.Runner.Version, tc.want)
			}
		})
	}
}

func TestConfigScalingDefaults(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	const scaling = "    scaling:\n      minRunners: 0\n      maxRunners: 2\n"
	for _, tc := range []struct {
		name, input string
		want        settings.ScalingConfig
	}{
		{"omitted", "", settings.ScalingConfig{MaxRunners: 2}},
		{"empty", "    scaling: {}\n", settings.ScalingConfig{MaxRunners: 2}},
		{"minimum only", "    scaling:\n      minRunners: 1\n", settings.ScalingConfig{MinRunners: 1, MaxRunners: 2}},
		{"maximum only", "    scaling:\n      maxRunners: 4\n", settings.ScalingConfig{MaxRunners: 4}},
		{"explicit", "    scaling:\n      minRunners: 1\n      maxRunners: 3\n", settings.ScalingConfig{MinRunners: 1, MaxRunners: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(testYAML, scaling, tc.input, 1)
			c, err := settings.LoadConfig(writeConfig(t, content))
			if err != nil {
				t.Fatal(err)
			}
			if c.Targets[0].Scaling != tc.want {
				t.Fatalf("got %+v, want %+v", c.Targets[0].Scaling, tc.want)
			}
		})
	}
}

func TestConfigScalingAliasDefaults(t *testing.T) {
	content := readFixture(t, "config/scaling-alias.yaml")
	c, err := settings.LoadConfig(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range c.Targets {
		if target.Scaling != (settings.ScalingConfig{MaxRunners: 2}) {
			t.Fatalf("unexpected aliased scaling defaults: %+v", target.Scaling)
		}
	}
}

func TestConfigTargetScope(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
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
		{"/owner", ""},
		{"/owner/repository", ""},
		{"//owner", ""},
		{"//owner/repository", ""},
		{"https://github.com//owner", ""},
		{"https://github.com//owner/repository", ""},
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
			c, err := settings.LoadConfig(writeConfig(t, content))
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

func TestConfigScaleSetName(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	for _, tc := range []struct {
		name, scaleSetName, want string
	}{
		{"omitted", "", ""},
		{"empty", `""`, ""},
		{"explicit", "remote", "remote"},
		{"non-ID name", "build/linux", "build/linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(testYAML, "    scaleSet: build\n", "", 1)
			if tc.scaleSetName != "" {
				content = strings.Replace(content, "    scaling:", "    scaleSet: "+tc.scaleSetName+"\n    scaling:", 1)
			}
			c, err := settings.LoadConfig(writeConfig(t, content))
			if tc.want == "" {
				if err == nil {
					t.Fatal("missing or empty scaleSet accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantKey, _ := settings.TargetKey(c.Targets[0].URL, "", tc.want)
			if c.Targets[0].ID != wantKey || c.Targets[0].ScaleSetName != tc.want {
				t.Fatalf("unexpected target identity: %+v", c.Targets[0])
			}
		})
	}
}

func TestConfigScaleSetNameRegistrations(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	const extra = `
  - scope: owner/repository
    credential:
      type: gh
    scaleSet: build
    scaling:
      maxRunners: 2
runner:`
	for _, tc := range []struct {
		name, scope string
		valid       bool
	}{
		{"same scope", "owner/repository", false},
		{"different scopes", "owner/another-repository", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := testYAML
			second := strings.Replace(extra, "owner/repository", tc.scope, 1)
			content = strings.Replace(content, "\nrunner:", second, 1)
			c, err := settings.LoadConfig(writeConfig(t, content))
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "duplicate scope/group/scale set registration") {
					t.Fatalf("expected duplicate registration, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Targets[0].ID == c.Targets[1].ID ||
				c.Targets[0].ScaleSetName != "build" || c.Targets[1].ScaleSetName != "build" {
				t.Fatalf("unexpected target identities: %+v", c.Targets)
			}
		})
	}
}

func TestConfigRejectsDuplicateShorthandRegistration(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	const extra = `
  - scope: REGISTRATION
    credential:
      type: gh
    scaleSet: build
    scaling:
      maxRunners: 2
runner:`
	for _, registration := range []string{"owner", "owner/repository"} {
		for _, tc := range []struct {
			prefix, want string
		}{
			{"https://github.com/", "duplicate scope/group/scale set registration"},
			{"/", ""},
			{"https://github.com//", ""},
		} {
			t.Run(registration+"/"+tc.prefix, func(t *testing.T) {
				content := strings.Replace(testYAML, "https://github.com/owner/repository", registration, 1)
				content = strings.Replace(content, "\nrunner:", strings.Replace(extra, "REGISTRATION", tc.prefix+registration, 1), 1)
				_, err := settings.LoadConfig(writeConfig(t, content))
				if err == nil {
					t.Fatal("duplicate or invalid registration was accepted")
				}
				if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("expected %q, got %v", tc.want, err)
				}
			})
		}
	}
}

func TestConfigAliases(t *testing.T) {
	input := readFixture(t, "config/aliases.yaml.tmpl")
	for _, tc := range []struct {
		name       string
		credential string
		app        bool
	}{
		{"default gh", "      hostname: github.com", false},
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
			c, err := settings.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			first, second := c.Targets[0], c.Targets[1]
			if first.Credential != second.Credential || first.Scaling != second.Scaling ||
				first.URL != "https://github.com/first" || (!tc.app && first.URL != second.URL) {
				t.Fatal("aliases did not expand into target values", c.Targets)
			}
			if tc.name == "default gh" && first.Credential.Type != "gh" {
				t.Fatal("aliased credential lost default type", first.Credential)
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
		credential     settings.Credential
		installationID int64
		valid          bool
	}{
		{"gh", settings.Credential{Type: "gh"}, 0, true},
		{"gh host", settings.Credential{Type: "gh", Hostname: "github.com"}, 0, true},
		{"wrong gh host", settings.Credential{Type: "gh", Hostname: "ghe.example.com"}, 0, false},
		{"gh with PAT", settings.Credential{Type: "gh", TokenEnv: "TOKEN"}, 0, false},
		{"PAT file", settings.Credential{Type: "pat", TokenFile: "/private/token"}, 0, true},
		{"PAT invalid env", settings.Credential{Type: "pat", TokenEnv: "BAD-NAME"}, 0, false},
		{"PAT host", settings.Credential{Type: "pat", TokenEnv: "TOKEN", Hostname: "github.com"}, 0, false},
		{"App", settings.Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, 100, true},
		{"App missing installation", settings.Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, 0, false},
		{"App negative installation", settings.Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem"}, -1, false},
		{"App missing key", settings.Credential{Type: "github-app", ClientID: "app"}, 100, false},
		{"App with PAT", settings.Credential{Type: "github-app", ClientID: "app", PrivateKeyFile: "/private/app.pem", TokenEnv: "TOKEN"}, 100, false},
		{"unknown type", settings.Credential{Type: "other"}, 0, false},
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
	testYAML := readFixture(t, "config/base.yaml")
	cases := map[string]string{
		"unknown key":              testYAML + "unknown: true\n",
		"nested unknown key":       strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      unknown: true", 1),
		"duplicate YAML key":       testYAML + "targets: []\n",
		"nested duplicate key":     strings.Replace(testYAML, "minRunners: 0", "minRunners: 0\n      minRunners: 1", 1),
		"multiple documents":       testYAML + "---\ntargets: []\n",
		"empty extra document":     testYAML + "---\n",
		"removed apiVersion":       testYAML + "apiVersion: ghalet/v1alpha1\n",
		"unfixed runner":           strings.Replace(testYAML, `"2.338.0"`, "latest", 1),
		"empty runner version":     strings.Replace(testYAML, `"2.338.0"`, `""`, 1),
		"empty credential type":    strings.Replace(testYAML, "    credential:\n      type: pat\n      tokenEnv: GHALET_TEST_TOKEN\n", "    credential:\n      type: \"\"\n", 1),
		"PAT without type":         strings.Replace(testYAML, "      type: pat\n", "", 1),
		"App without type":         strings.Replace(testYAML, "      type: pat\n      tokenEnv: GHALET_TEST_TOKEN", "      clientId: app\n      privateKeyFile: credentials/app.pem", 1),
		"unknown credential key":   strings.Replace(testYAML, "      type: pat\n      tokenEnv: GHALET_TEST_TOKEN", "      unknown: true", 1),
		"duplicate credential key": strings.Replace(testYAML, "      type: pat", "      type: pat\n      type: gh", 1),
		"missing scope":            strings.Replace(testYAML, "scope: https://github.com/owner/repository", "scope: \"\"", 1),
		"multiple scopes":          strings.Replace(testYAML, "scope: https://github.com/owner/repository", "scope: [owner/repository, owner/another-repository]", 1),
		"old URL key":              strings.Replace(testYAML, "scope: https://github.com/owner/repository", "url: https://github.com/owner/repository", 1),
		"legacy credentials":       testYAML + "credentials: {}\n",
		"legacy scopes":            testYAML + "scopes: {}\n",
		"legacy scopeRef":          strings.Replace(testYAML, "scaleSet: build", "scaleSet: build\n    scopeRef: repository", 1),
		"legacy credentialRef":     strings.Replace(testYAML, "scaleSet: build", "scaleSet: build\n    credentialRef: personal", 1),
		"old scale set key":        strings.Replace(testYAML, "scaleSet: build", "scaleSetName: build", 1),
		"repository group":         strings.Replace(testYAML, "scaleSet: build", "runnerGroup: special\n    scaleSet: build", 1),
		"enterprise":               strings.Replace(testYAML, "owner/repository", "enterprises/company", 1),
		"HTTP scope":               strings.Replace(testYAML, "https:", "http:", 1),
		"userinfo":                 strings.Replace(testYAML, "github.com/", "user:secret@github.com/", 1),
		"query":                    strings.Replace(testYAML, "owner/repository", "owner/repository?token=abc", 1),
		"removed target id":        strings.Replace(testYAML, "scaleSet: build", "scaleSet: build\n    id: build", 1),
		"missing scale set":        strings.Replace(testYAML, "    scaleSet: build\n", "", 1),
		"empty scale set":          strings.Replace(testYAML, "scaleSet: build", `scaleSet: ""`, 1),
		"negative min":             strings.Replace(testYAML, "minRunners: 0", "minRunners: -1", 1),
		"min exceeds max":          strings.Replace(testYAML, "minRunners: 0", "minRunners: 3", 1),
		"zero max":                 strings.Replace(testYAML, "maxRunners: 2", "maxRunners: 0", 1),
		"min exceeds global":       strings.Replace(testYAML, "minRunners: 0", "minRunners: 2", 1) + "capacity:\n  maxRunners: 1\n",
		"zero log retention":       testYAML + "logs:\n  retention: 0s\n",
		"old cleanup namespace":    testYAML + "cleanup: {}\n",
		"old log preservation":     testYAML + "logs:\n  preserveLogs: false\n",
		"old log retention":        testYAML + "logs:\n  logRetention: 7d\n",
		"old log root":             testYAML + "logs:\n  logRoot: logs\n",
		"two PAT inputs":           strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "tokenEnv: GHALET_TEST_TOKEN\n      tokenFile: token", 1),
		"inline token":             strings.Replace(testYAML, "tokenEnv: GHALET_TEST_TOKEN", "token: secret", 1),
		"App installation":         strings.Replace(testYAML, "scaleSet: build", "scaleSet: build\n    installationId: 10", 1),
		"credential env":           strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    GH_TOKEN: never-copy", 1),
		"custom credential env":    strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    GHALET_TEST_TOKEN: never-copy", 1),
		"identity env":             strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    HOME: /tmp", 1),
		"removed workRoot":         strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  workRoot: runs", 1),
		"removed distributionRoot": strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  distributionRoot: runners", 1),
		"removed cacheRoot":        strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  cacheRoot: downloads", 1),
		"empty log root":           testYAML + "logs:\n  root: \"\"\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := settings.LoadConfig(writeConfig(t, input)); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestConfigRejectsRunnerInputEnvironmentCaseInsensitively(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
	for _, name := range []string{
		"ACTIONS_RUNNER_INPUT_HELP",
		"actions_runner_input_help",
		"Actions_Runner_Input_Help",
		"actions_runner_input_jitconfig",
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(testYAML, `"2.338.0"`, "\"2.338.0\"\n  env:\n    "+name+": \"true\"", 1)
			if _, err := settings.LoadConfig(writeConfig(t, input)); err == nil {
				t.Fatal("runner input override was accepted", name)
			}
		})
	}
}

func TestConfigDurationDecoding(t *testing.T) {
	testYAML := readFixture(t, "config/base.yaml")
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
			c, err := settings.LoadConfig(writeConfig(t, testYAML+"logs:\n  retention: "+tc.input+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			if c.Logs.Retention != settings.Duration(tc.want) {
				t.Fatalf("got %v, want %v", time.Duration(c.Logs.Retention), tc.want)
			}
		})
	}
	for _, input := range []string{"0s", "-1d", "invalid", "2562048d", "2562048h", "true", "[7d]", "{days: 7}", "1h1d", "2m1d", "1d2h"} {
		t.Run(input, func(t *testing.T) {
			if _, err := settings.LoadConfig(writeConfig(t, testYAML+"logs:\n  retention: "+input+"\n")); err == nil {
				t.Fatal("invalid duration was accepted")
			}
		})
	}
}

func TestConfigDuplicateRegistration(t *testing.T) {
	c := testConfig(t)
	c.Targets = append(c.Targets, c.Targets[0])
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate scope/group/scale set registration") {
		t.Fatalf("duplicate registration accepted: %v", err)
	}
}

func TestConfigValidationOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*settings.Config)
		want   string
	}{
		{"scope before credential", func(c *settings.Config) {
			c.Targets[0].URL, c.Targets[0].Credential.Type = "invalid", "invalid"
		}, "target " + buildTargetID + ": scope URL must be an HTTPS organization or repository URL"},
		{"credential before installation", func(c *settings.Config) {
			c.Targets[0].Credential.Type, c.Targets[0].InstallationID = "invalid", 1
		}, "target " + buildTargetID + `: unsupported credential type "invalid"`},
		{"target before total minimum", func(c *settings.Config) {
			c.Targets[0].Scaling.MinRunners = c.Capacity.MaxRunners + 1
		}, "target " + buildTargetID + ": require 0 <= minRunners <= maxRunners <= 10000"},
		{"total minimum before environment", func(c *settings.Config) {
			c.Targets[0].Scaling = settings.ScalingConfig{MinRunners: c.Capacity.MaxRunners + 1, MaxRunners: 10000}
			c.Runner.Env = map[string]string{"HOME": "/tmp"}
		}, "sum of minRunners exceeds capacity.maxRunners"},
		{"environment before directories", func(c *settings.Config) {
			c.Runner.Env = map[string]string{"HOME": "/tmp"}
			c.Runner.WorkRoot = "/"
		}, `runner.env cannot set invalid, identity or management credential variable "HOME"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(c)
			if err := c.Validate(); err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestScopeURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantErr   bool
	}{
		{"organization", "https://github.com/org", false},
		{"repository", "https://github.com/org/repo", false},
		{"enterprise host", "https://ghe.example.com/org/repo", false},
		{"missing owner", "https://github.com", true},
		{"parent traversal", "https://github.com/a/../b", true},
		{"extra path", "https://github.com/a/b/c", true},
		{"encoded slash", "https://github.com/a%2fb", true},
		{"explicit port", "https://github.com:443/org", true},
		{"enterprise scope", "https://github.com/Enterprises/acme", true},
		{"uppercase enterprise scope", "https://github.com/ENTERPRISES/acme", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := settings.ScopeURL(tc.raw); (err != nil) != tc.wantErr {
				t.Fatalf("scopeURL(%q): got %v, want error=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}
