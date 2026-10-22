package ghalet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/logreduct"
	"github.com/actions/scaleset"
)

func TestCredentialsAndSecretRedaction(t *testing.T) {
	c := testConfig(t)
	secret := &logreduct.Redactor{}
	t.Setenv("GHALET_TEST_TOKEN", "management-secret")
	client, err := newScaleClient(t.Context(), c.Targets[0], secret)
	if err != nil || client == nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{".credentials": base64.StdEncoding.EncodeToString([]byte("runner-credential"))})
	jit := base64.StdEncoding.EncodeToString(raw)
	secret.AddJIT(jit)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: secret.ReplaceAttr}))
	logger.Info("management-secret\nmultiline", "jit", jit, "error", errors.New("runner-credential"))
	if strings.Contains(output.String(), "management-secret") || strings.Contains(output.String(), jit) ||
		strings.Contains(output.String(), "runner-credential") || bytes.Count(output.Bytes(), []byte("\n")) != 1 || !json.Valid(bytes.TrimSpace(output.Bytes())) {
		t.Fatalf("secret or invalid JSONL output: %s", output.String())
	}
	t.Setenv("GHALET_TEST_TOKEN", "")
	if _, err := newScaleClient(t.Context(), c.Targets[0], secret); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestGlobalLoggerRedactsSecrets(t *testing.T) {
	var output bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: logreduct.Default.ReplaceAttr})))
	defer slog.SetDefault(original)
	values := []string{"management-secret", "jit-secret"}
	for _, value := range values {
		logreduct.Default.Add(value)
	}
	defer logreduct.Default.Release(values)
	slog.With("credential", "management-secret").WithGroup("execution").Info("jit-secret",
		slog.Group("details", "error", errors.New("jit-secret"), "token", "management-secret"))
	if strings.Contains(output.String(), "management-secret") || strings.Contains(output.String(), "jit-secret") ||
		!json.Valid(bytes.TrimSpace(output.Bytes())) {
		t.Fatal("configured logger leaked a secret or invalid JSON", output.String())
	}
	if !strings.Contains(output.String(), `"credential":"***"`) || !strings.Contains(output.String(), `"execution":`) {
		t.Fatal("logger lost structured attributes", output.String())
	}
}

func TestGlobalLogRedactionTracksJITLifetime(t *testing.T) {
	var output bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: logreduct.Default.ReplaceAttr})))
	defer slog.SetDefault(original)
	value := base64.StdEncoding.EncodeToString([]byte("global-log-jit-lifetime-secret"))
	values := logreduct.Default.AddJIT(value)
	slog.Info("Runner failed", "jit", value)
	redacted := output.String()
	logreduct.Default.Release(values)
	if strings.Contains(redacted, value) || !strings.Contains(redacted, `"jit":"***"`) {
		t.Fatal("new JIT secret was not redacted", redacted)
	}
	output.Reset()
	slog.Info("Runner cleaned", "jit", value)
	if !strings.Contains(output.String(), value) {
		t.Fatal("released JIT secret remained in the registry", output.String())
	}
}

func TestDirectAPILogRedactionWithOrdinaryHandler(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("daemon entrypoints require a non-root execution user")
	}
	const credential = "direct-api-management-secret"
	t.Setenv("GH_TOKEN", credential)
	for _, command := range []string{"run", "gc"} {
		t.Run(command, func(t *testing.T) {
			data := strings.Replace(readFixture(t, "config/base.yaml"), "tokenEnv: GHALET_TEST_TOKEN", "tokenFile: missing-"+credential, 1)
			data = strings.Replace(data, "scaleSet: build", "scaleSet: "+credential, 1)
			path := writeConfig(t, data)
			config, err := settings.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(socketPath(path) + ".lock") })
			var output bytes.Buffer
			original := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(original)
			defer logreduct.Default.Release([]string{credential})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch command {
			case "run":
				cancel()
				err = RunDaemon(ctx, config)
			case "gc":
				_, err = GC(ctx, path, true)
			}
			if command == "run" && err == nil {
				t.Fatal("canceled operation with a missing credential file succeeded")
			}
			if command == "gc" && err != nil {
				t.Fatal("offline dry-run garbage collection failed", err)
			}
			if strings.Contains(output.String(), credential) {
				t.Fatal("direct API logging exposed a registered credential", output.String())
			}
			var record struct {
				Event    string `json:"event"`
				TargetID string `json:"targetId"`
				Error    string `json:"error"`
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal("expected a registration failure log", err, output.String())
			}
			if record.Event != "target.failed" || record.TargetID != config.Targets[0].ID || !strings.Contains(record.Error, "missing-***") {
				t.Fatal("redaction lost the failure diagnostics", record)
			}
		})
	}
}

func TestProtectedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		want    string
		wantErr bool
	}{
		{name: "world-readable", mode: 0644, wantErr: true},
		{name: "private", mode: 0600, want: "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(testDirectory(t), "token")
			if err := os.WriteFile(path, []byte(" secret \n"), tc.mode); err != nil {
				t.Fatal(err)
			}
			value, err := readProtected(path)
			if (err != nil) != tc.wantErr || value != tc.want {
				t.Fatalf("got %q, %v; want %q, error=%v", value, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestSharedAppKeyUsesTargetInstallations(t *testing.T) {
	c := testConfig(t)
	first := c.Targets[0]
	first.URL = "https://github.com/first"
	first.Credential = settings.Credential{Type: "github-app", ClientID: "app-client", PrivateKeyFile: "/protected/app.pem"}
	first.Credential.InstallationID = 100
	second := first
	second.ID, second.URL, second.Credential.InstallationID = "second", "https://github.com/second", 200
	c.Targets = []settings.Target{first, second}
	if err := c.Validate(); err != nil {
		t.Fatal("shared App key rejected across installations", err)
	}
	c.Targets[1].Credential.InstallationID = 0
	if err := c.Validate(); err == nil {
		t.Fatal("App installation identity was not target-specific")
	}
}

func TestGhAuthenticationUsesHostAndFiniteCommand(t *testing.T) {
	c := testConfig(t)
	dir := testDirectory(t)
	path := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = 'auth token --hostname github.com' ] || exit 4\nprintf fake-gh-secret\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	c.Targets[0].Credential = settings.Credential{Type: "gh"}
	secret := &logreduct.Redactor{}
	if _, err := newScaleClient(t.Context(), c.Targets[0], secret); err != nil {
		t.Fatal(err)
	}
	if secret.Redact("fake-gh-secret") != "***" {
		t.Fatal("gh token not registered for redaction")
	}
	os.WriteFile(path, []byte("#!/bin/sh\nprintf sensitive-stderr >&2\nexit 1\n"), 0700)
	if _, err := newScaleClient(t.Context(), c.Targets[0], secret); err == nil || strings.Contains(err.Error(), "sensitive-stderr") {
		t.Fatal("gh error leaked subprocess stderr or was ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newScaleClient(ctx, c.Targets[0], secret); err == nil {
		t.Fatal("canceled gh command succeeded")
	}
}

func TestExplicitRegistrationAndDisableUpdate(t *testing.T) {
	target := testConfig(t).Targets[0]
	for _, tc := range []struct {
		name                   string
		create                 bool
		missing                bool
		enableUpdate           bool
		getErr, groupErr       error
		wantErr                error
		wantSet                bool
		wantCreate, wantUpdate int
	}{
		{name: "reuse existing", create: true, wantSet: true},
		{name: "disable runner updates", create: true, enableUpdate: true, wantSet: true, wantUpdate: 1},
		{name: "lookup missing", missing: true},
		{name: "create missing", create: true, missing: true, wantSet: true, wantCreate: 1},
		{name: "scale set lookup failure", create: true, getErr: errTestAPI, wantErr: errTestAPI},
		{name: "group lookup failure", create: true, getErr: errTestAPI, groupErr: errTestAPI, wantErr: errTestAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.set.RunnerSetting.DisableUpdate = !tc.enableUpdate
			if tc.missing {
				api.set = nil
			}
			api.getErr, api.groupErr = tc.getErr, tc.groupErr
			set, err := registration(t.Context(), api, target, tc.create)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got error %v, want %v", err, tc.wantErr)
			}
			if (set != nil) != tc.wantSet {
				t.Fatalf("got scale set %+v, want present=%v", set, tc.wantSet)
			}
			if set != nil && (set.ID != 10 || !set.RunnerSetting.DisableUpdate) {
				t.Fatalf("unexpected registration: %+v", set)
			}
			if api.createCalls != tc.wantCreate || api.updateCalls != tc.wantUpdate {
				t.Fatalf("got create=%d, update=%d; want create=%d, update=%d",
					api.createCalls, api.updateCalls, tc.wantCreate, tc.wantUpdate)
			}
		})
	}
}

func TestRunnerEnvironment(t *testing.T) {
	c := testConfig(t)
	for key, value := range map[string]string{
		"PATH": "/controller/bin", "HOME": "/home/controller", "USER": "controller", "LOGNAME": "controller",
		"GH_TOKEN": "management", "GITHUB_TOKEN": "management", "GHALET_TEST_TOKEN": "management",
		"AWS_SECRET_ACCESS_KEY": "unlisted-secret", "NPM_TOKEN": "unlisted-secret",
		"SSH_AUTH_SOCK": "/controller/agent", "HTTPS_PROXY": "http://controller.invalid",
		"LANG": "controller-locale", "TMPDIR": "/controller/tmp", "KEEP": "inherited",
		"GH_CONFIG_DIR": "/controller/gh", "XDG_CONFIG_HOME": "/controller/config",
		"LD_PRELOAD": "/controller/preload", "DYLD_INSERT_LIBRARIES": "/controller/preload",
		"ACTIONS_RUNNER_INPUT_OLD": "secret", "RUNNER_ALLOW_RUNASROOT": "1",
		"actions_runner_input_help": "true", "Actions_Runner_Input_Help": "true",
		"actions_runner_input_jitconfig": "inherited-jit",
	} {
		t.Setenv(key, value)
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if runtime.GOOS == "darwin" {
		path = "/opt/homebrew/bin:/opt/homebrew/sbin:" + path
	}
	for _, account := range []executionUser{
		{Name: "controller", Home: "/home/controller"},
		{Name: "runner", Home: "/home/runner"},
	} {
		for _, jit := range []string{"", "jit-only"} {
			t.Run(account.Name+"/"+jit, func(t *testing.T) {
				environment := runnerEnvironment(c, account, jit)
				want := []string{
					"PATH=" + path, "HOME=" + account.Home,
					"USER=" + account.Name, "LOGNAME=" + account.Name,
				}
				if jit != "" {
					want = append(want, "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit)
				}
				slices.Sort(environment)
				slices.Sort(want)
				if !slices.Equal(environment, want) {
					t.Fatalf("got %q, want %q", environment, want)
				}
			})
		}
	}
}

func TestRunnerEnvironmentOverrides(t *testing.T) {
	c := testConfig(t)
	other := c.Targets[0]
	other.ID, other.ScaleSetName = "other", "other"
	other.Credential.TokenEnv = "OTHER_MANAGEMENT_TOKEN"
	c.Targets = append(c.Targets, other)
	c.Runner.Env = map[string]string{
		"PATH": "/tools/bin", "LANG": "C.UTF-8",
		"HTTPS_PROXY": "http://proxy.invalid", "KEEP": "explicit",
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	account := executionUser{Name: "runner", UID: 1001, Home: "/home/runner"}
	environment := runnerEnvironment(c, account, "jit-only")
	want := []string{
		"PATH=/tools/bin", "LANG=C.UTF-8", "HTTPS_PROXY=http://proxy.invalid", "KEEP=explicit",
		"HOME=/home/runner", "USER=runner", "LOGNAME=runner", "ACTIONS_RUNNER_INPUT_JITCONFIG=jit-only",
	}
	slices.Sort(environment)
	slices.Sort(want)
	if !slices.Equal(environment, want) {
		t.Fatalf("got %q, want %q", environment, want)
	}
	c.Runner.Env["OTHER_MANAGEMENT_TOKEN"] = "must-not-be-copied"
	if err := c.Validate(); err == nil {
		t.Fatal("runner.env accepted another target's management credential")
	}
}

func TestPermanentAPIErrorsDoNotRetry(t *testing.T) {
	calls := 0
	err := retry(t.Context(), func() error { calls++; return scaleset.UnauthorizedError })
	if !errors.Is(err, scaleset.UnauthorizedError) || calls != 1 {
		t.Fatal("permanent auth error was retried")
	}
}
