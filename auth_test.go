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
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

func TestCredentialsAndSecretRedaction(t *testing.T) {
	c := testConfig(t)
	secret := &secrets{}
	t.Setenv("GHALET_TEST_TOKEN", "management-secret")
	client, err := newScaleClient(t.Context(), c, "repository", secret)
	if err != nil || client == nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{".credentials": base64.StdEncoding.EncodeToString([]byte("runner-credential"))})
	jit := base64.StdEncoding.EncodeToString(raw)
	secret.addJIT(jit)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{ReplaceAttr: secret.replaceAttr}))
	logger.Info("management-secret\nmultiline", "jit", jit, "error", errors.New("runner-credential"))
	if strings.Contains(output.String(), "management-secret") || strings.Contains(output.String(), jit) ||
		strings.Contains(output.String(), "runner-credential") || bytes.Count(output.Bytes(), []byte("\n")) != 1 || !json.Valid(bytes.TrimSpace(output.Bytes())) {
		t.Fatalf("secret or invalid JSONL output: %s", output.String())
	}
	t.Setenv("GHALET_TEST_TOKEN", "")
	if _, err := newScaleClient(t.Context(), c, "repository", secret); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestProtectedCredentials(t *testing.T) {
	path := filepath.Join(testDirectory(t), "token")
	if err := os.WriteFile(path, []byte(" secret \n"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := readProtected(path); err == nil {
		t.Fatal("world-readable secret accepted")
	}
	os.Chmod(path, 0600)
	value, err := readProtected(path)
	if err != nil || value != "secret" {
		t.Fatalf("%q, %v", value, err)
	}
}

func TestMultilineSecretReferenceCounts(t *testing.T) {
	secret := &secrets{}
	first := "shared-line\r\nfirst-line\r\nshared-line\r\n"
	second := "shared-line\nsecond-line"
	secret.add(first)
	secret.add(first)
	secret.add(second)
	secret.add("first-line")
	secret.release([]string{first})
	if got := secret.redact("shared-line first-line second-line"); got != "*** *** ***" {
		t.Fatal("releasing one reference exposed shared components", got)
	}
	secret.release([]string{first})
	if got := secret.redact("shared-line first-line second-line"); got != "*** *** ***" {
		t.Fatal("releasing a multiline secret exposed independently registered components", got)
	}
	secret.release([]string{second})
	if got := secret.redact("shared-line first-line second-line"); got != "shared-line *** second-line" {
		t.Fatal("multiline components were not released independently", got)
	}
	secret.release([]string{"first-line"})
	if len(secret.values) != 0 || len(secret.refs) != 0 {
		t.Fatal("released multiline secrets retained registered components")
	}

	raw, err := json.Marshal(map[string]string{"credential": base64.StdEncoding.EncodeToString([]byte(first))})
	if err != nil {
		t.Fatal(err)
	}
	values := secret.addJIT(base64.StdEncoding.EncodeToString(raw))
	if got := secret.redact("shared-line first-line"); got != "*** ***" {
		t.Fatal("decoded multiline JIT secret was not registered", got)
	}
	secret.release(values)
	if len(secret.values) != 0 || len(secret.refs) != 0 {
		t.Fatal("JIT release retained multiline components")
	}
}

func TestSharedAppKeyUsesScopeInstallations(t *testing.T) {
	c := testConfig(t)
	c.Credentials["app"] = Credential{Type: "github-app", ClientID: "app-client", PrivateKeyFile: "/protected/app.pem"}
	first := Scope{URL: "https://github.com/first", CredentialRef: "app", InstallationID: 100}
	second := Scope{URL: "https://github.com/second", CredentialRef: "app", InstallationID: 200}
	c.Scopes["first"], c.Scopes["second"] = first, second
	if err := c.Validate(); err != nil {
		t.Fatal("shared App key rejected across installations", err)
	}
	second.InstallationID = 0
	c.Scopes["second"] = second
	if err := c.Validate(); err == nil {
		t.Fatal("App installation identity was not scope-specific")
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
	c.Credentials["personal"] = Credential{Type: "gh"}
	secret := &secrets{}
	if _, err := newScaleClient(t.Context(), c, "repository", secret); err != nil {
		t.Fatal(err)
	}
	if secret.redact("fake-gh-secret") != "***" {
		t.Fatal("gh token not registered for redaction")
	}
	os.WriteFile(path, []byte("#!/bin/sh\nprintf sensitive-stderr >&2\nexit 1\n"), 0700)
	if _, err := newScaleClient(t.Context(), c, "repository", secret); err == nil || strings.Contains(err.Error(), "sensitive-stderr") {
		t.Fatal("gh error leaked subprocess stderr or was ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newScaleClient(ctx, c, "repository", secret); err == nil {
		t.Fatal("canceled gh command succeeded")
	}
}

func TestExplicitRegistrationAndDisableUpdate(t *testing.T) {
	api := newFakeAPI()
	target := testConfig(t).Targets[0]
	set, err := registration(t.Context(), api, target, true)
	if err != nil || set.ID != 10 || api.createCalls != 0 {
		t.Fatal("existing registration was not reused")
	}
	api.set.RunnerSetting.DisableUpdate = false
	if _, err := registration(t.Context(), api, target, true); err != nil || api.updateCalls != 1 || !api.set.RunnerSetting.DisableUpdate {
		t.Fatal("runner update was not disabled")
	}
	api.set = nil
	if _, err := registration(t.Context(), api, target, false); err != nil || api.createCalls != 0 {
		t.Fatal("lookup-only operation created a scale set")
	}
	if _, err := registration(t.Context(), api, target, true); err != nil || api.createCalls != 1 {
		t.Fatal("missing explicit registration was not created")
	}
	api.getErr = errTestAPI
	if _, err := registration(t.Context(), api, target, true); err == nil || api.createCalls != 1 {
		t.Fatal("API failure interpreted as missing registration")
	}
	api.groupErr = errTestAPI
	if _, err := registration(t.Context(), api, target, true); err == nil || api.createCalls != 1 {
		t.Fatal("group failure fell back or created a registration")
	}
}

func TestRunnerEnvironment(t *testing.T) {
	c := testConfig(t)
	c.Runner.Env = map[string]string{"PATH": "/tools/bin", "LANG": "C.UTF-8"}
	account := executionUser{Name: "runner", UID: 1001, Home: "/home/runner"}
	environment := runnerEnvironment(c, account, []string{
		"GH_TOKEN=management", "GITHUB_TOKEN=management", "GHALET_TEST_TOKEN=management",
		"PATH=/old", "HOME=/controller", "USER=controller", "LOGNAME=controller",
		"KEEP=value", "ACTIONS_RUNNER_INPUT_OLD=secret", "RUNNER_ALLOW_RUNASROOT=1",
	}, "jit-only")
	all := "\n" + strings.Join(environment, "\n") + "\n"
	for _, forbidden := range []string{"management", "/controller", "ACTIONS_RUNNER_INPUT_OLD", "RUNNER_ALLOW_RUNASROOT"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("forbidden inherited environment: %s", all)
		}
	}
	for _, expected := range []string{"PATH=/tools/bin", "HOME=/home/runner", "USER=runner", "LOGNAME=runner", "KEEP=value", "ACTIONS_RUNNER_INPUT_JITCONFIG=jit-only"} {
		if !strings.Contains(all, "\n"+expected+"\n") {
			t.Fatalf("missing %s", expected)
		}
	}
}

func TestPermanentAPIErrorsDoNotRetry(t *testing.T) {
	calls := 0
	err := retry(t.Context(), func() error { calls++; return scaleset.UnauthorizedError })
	if !errors.Is(err, scaleset.UnauthorizedError) || calls != 1 {
		t.Fatal("permanent auth error was retried")
	}
}
