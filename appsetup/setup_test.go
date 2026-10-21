package appsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sys/unix"
)

type fixture struct {
	service
	options          Options
	key              *rsa.PrivateKey
	pem              string
	ownerType        string
	scope            string
	appOwner         string
	permission       string
	installed        atomic.Bool
	conversions      atomic.Int32
	ownerCalls       atomic.Int32
	status           int
	conversionStatus int
	localURL         string
	callbackHTML     string
	callbackStatus   int
	browserCalls     atomic.Int32
	onInstall        func()
	onLookup         func()
}

func setupFixture(t *testing.T, scope, ownerType string) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_CACHE_HOME", root)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		key: key, ownerType: ownerType, scope: scope, appOwner: "owner", permission: "write",
		pem:     string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		options: Options{ID: "build", Scope: scope, Config: filepath.Join(root, "config.yaml"), Yes: true},
	}
	data := "# keep this comment\ndefaults:\n  credential: &shared\n    type: gh\ntargets:\n  - id: build\n    scope: " + scope + "\n  - id: other\n    scope: owner/other\n    credential: *shared\n"
	if err := os.WriteFile(f.options.Config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(f.apiHandler(t)))
	t.Cleanup(server.Close)
	f.service = service{
		api:  apiClient{base: server.URL, http: server.Client()},
		open: f.browser(t), saveKey: credentialstore.SetFile,
		prepare:  func(context.Context) (string, error) { return credentialstore.PrepareDirectory() },
		readKey:  func(_ context.Context, path string) ([]byte, error) { return readPrivate(path) },
		interval: time.Millisecond, timeout: 2 * time.Second,
	}
	return f
}

func (f *fixture) apiHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/owner":
			f.ownerCalls.Add(1)
			json.NewEncoder(w).Encode(account{Login: "owner", Type: f.ownerType})
		case r.URL.Path == "/app-manifests/test-code/conversions":
			f.conversions.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
				t.Error("conversion required a token or wrong method")
			}
			if f.conversionStatus != 0 {
				w.WriteHeader(f.conversionStatus)
				fmt.Fprint(w, "sensitive-exchange-error")
				return
			}
			json.NewEncoder(w).Encode(app{ID: 77, ClientID: "Iv1.test", Slug: "ghalet-test", Owner: account{Login: f.appOwner}, PEM: f.pem})
		case strings.HasSuffix(r.URL.Path, "/installation"):
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parsed, err := jwt.Parse(token, func(token *jwt.Token) (any, error) { return &f.key.PublicKey, nil },
				jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("Iv1.test"))
			if err != nil || !parsed.Valid {
				t.Error("App JWT verification failed")
			}
			if r.URL.Path != installationPath(f.scope) {
				t.Error("installation lookup used wrong scope")
			}
			if f.onLookup != nil {
				f.onLookup()
			}
			if f.status != 0 {
				w.WriteHeader(f.status)
				fmt.Fprint(w, "response-secret-must-not-leak")
				return
			}
			if !f.installed.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			permission := "administration"
			if !strings.Contains(f.scope, "/") {
				permission = "organization_self_hosted_runners"
			}
			json.NewEncoder(w).Encode(installation{
				ID: 123, AppID: 77, Account: account{Login: "owner"},
				Permissions: map[string]string{permission: f.permission},
			})
		default:
			t.Errorf("unexpected API request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

type confirmationOutput struct {
	io.Writer
	onPrompt func()
}

func (output confirmationOutput) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("Write these configuration changes?")) {
		output.onPrompt()
	}
	return output.Writer.Write(data)
}

func confirmationInput(t *testing.T, answer string) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	defer writer.Close()
	if _, err := io.WriteString(writer, answer); err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestSetupRejectsKeyReplacementAfterVerification(t *testing.T) {
	for _, stage := range []string{"polling", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			before, _ := os.ReadFile(f.options.Config)
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			replacement := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
			directory, err := credentialstore.Directory()
			if err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(directory, f.options.ID+".pem")
			rotate := func() {
				_, err := credentialstore.SetFile(t.Context(), credentialstore.Options{
					Name: f.options.ID, Type: "github-app", Stdin: true, NoDefault: true,
				}, bytes.NewReader(replacement))
				if err != nil {
					t.Error("could not perform concurrent key replacement", err)
				}
			}
			var input *os.File
			var buffer bytes.Buffer
			var output io.Writer = &buffer
			if stage == "polling" {
				f.onLookup = rotate
			} else {
				f.options.Yes = false
				input = confirmationInput(t, "yes\n")
				output = confirmationOutput{Writer: &buffer, onPrompt: rotate}
			}
			_, err = f.run(t.Context(), f.options, input, output)
			if err == nil || !strings.Contains(err.Error(), "key changed during setup") {
				t.Fatal("setup committed an unverified replacement key", err)
			}
			after, _ := os.ReadFile(f.options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("key replacement failure changed configuration")
			}
			current, err := os.ReadFile(keyPath)
			if err != nil || !bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(replacement)) {
				t.Fatal("setup overwrote the concurrently replaced key", err)
			}
		})
	}
}

func TestSetupLockReportsContention(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockSetup(directory, "build")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	second, err := lockSetup(directory, "build")
	if second != nil {
		second.Close()
		t.Fatal("second setup lock succeeded")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) || !strings.Contains(err.Error(), "another setup for this credential is in progress") {
		t.Fatal("setup lock lost its contention identity or context:", err)
	}
}

func TestSetupFinalCommitHonorsCredentialLock(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	before, _ := os.ReadFile(f.options.Config)
	f.onInstall = func() {
		directory, err := credentialstore.Directory()
		if err != nil {
			t.Fatal(err)
		}
		lock, err := credentialstore.LockDirectory(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { lock.Close() })
		f.installed.Store(true)
	}
	var output bytes.Buffer
	if _, err := f.run(t.Context(), f.options, nil, &output); err == nil {
		t.Fatal("setup committed while credential replacement held the store lock")
	}
	after, _ := os.ReadFile(f.options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("failed credential lock changed configuration")
	}
}

func (f *fixture) browser(t *testing.T) func(context.Context, string) error {
	t.Helper()
	return func(ctx context.Context, address string) error {
		f.browserCalls.Add(1)
		if strings.HasPrefix(address, "https://github.com/apps/") {
			if f.onInstall != nil {
				f.onInstall()
			} else {
				f.installed.Store(true)
			}
			return nil
		}
		f.localURL = address
		response, err := http.Get(address)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return err
		}
		matches := regexp.MustCompile(`name="manifest" value="([^"]+)"`).FindSubmatch(data)
		if len(matches) != 2 {
			t.Fatalf("missing Manifest form: %s", data)
		}
		var manifest manifest
		if err := json.Unmarshal([]byte(html.UnescapeString(string(matches[1]))), &manifest); err != nil {
			t.Fatal(err)
		}
		permission := "administration"
		if !strings.Contains(f.scope, "/") {
			permission = "organization_self_hosted_runners"
		}
		if manifest.Public || manifest.Hook.Active || len(manifest.Events) != 0 ||
			len(manifest.Permissions) != 1 || manifest.Permissions[permission] != "write" {
			t.Fatal("Manifest requests unexpected access", manifest)
		}
		action := regexp.MustCompile(`action="([^"]+)"`).FindSubmatch(data)
		actionURL, err := url.Parse(html.UnescapeString(string(action[1])))
		if err != nil {
			t.Fatal(err)
		}
		want := "/settings/apps/new"
		if f.ownerType == "Organization" {
			want = "/organizations/owner/settings/apps/new"
		}
		if actionURL.Host != "github.com" || actionURL.Path != want {
			t.Fatal("incorrect registration owner URL")
		}
		callback := manifest.RedirectURL + "?code=test-code&state=" + actionURL.Query().Get("state")
		bad, err := http.Get(manifest.RedirectURL + "?code=test-code&state=invalid")
		if err != nil {
			return err
		}
		bad.Body.Close()
		if bad.StatusCode != http.StatusBadRequest {
			t.Error("callback accepted invalid state")
		}
		duplicate, err := http.Get(callback + "&code=duplicate")
		if err != nil {
			return err
		}
		duplicate.Body.Close()
		if duplicate.StatusCode != http.StatusBadRequest {
			t.Error("callback accepted duplicate code parameters")
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		req.Host = "attacker.invalid"
		badHost, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		badHost.Body.Close()
		if badHost.StatusCode != http.StatusForbidden {
			t.Error("local listener accepted an unrelated Host")
		}
		finished, err := http.Get(callback)
		if err != nil {
			return err
		}
		defer finished.Body.Close()
		body, err := io.ReadAll(finished.Body)
		if err != nil {
			return err
		}
		f.callbackHTML = string(body)
		f.callbackStatus = finished.StatusCode
		if finished.StatusCode == http.StatusOK {
			if !strings.Contains(f.callbackHTML, `href="https://github.com/apps/ghalet-test/installations/new"`) {
				t.Error("callback did not link to the created App installation")
			}
			if f.onInstall != nil {
				f.onInstall()
			} else {
				f.installed.Store(true)
			}
		}
		return nil
	}
}

func TestManifestSetup(t *testing.T) {
	for _, tc := range []struct{ scope, ownerType string }{
		{"owner/repo", "User"}, {"owner/repo", "Organization"}, {"owner", "Organization"},
	} {
		t.Run(tc.scope+tc.ownerType, func(t *testing.T) {
			f := setupFixture(t, tc.scope, tc.ownerType)
			var output bytes.Buffer
			result, err := f.run(t.Context(), f.options, nil, &output)
			if err != nil {
				t.Fatal(err, output.String())
			}
			config, err := settings.LoadConfig(f.options.Config)
			if err != nil {
				t.Fatal(err)
			}
			target := config.Targets[0]
			if result.InstallationID != 123 || target.InstallationID != 123 ||
				target.Credential.ClientID != "Iv1.test" || target.Credential.PrivateKeyFile != result.KeyPath ||
				config.Targets[1].Credential.Type != "gh" || config.Defaults.Credential.Type != "gh" {
				t.Fatal("target update changed unrelated credentials", config)
			}
			data, _ := os.ReadFile(f.options.Config)
			if !bytes.Contains(data, []byte("# keep this comment")) {
				t.Fatal("configuration comment was lost")
			}
			for _, path := range []string{result.KeyPath, result.MetadataPath} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("unsafe saved file mode", path, err)
				}
			}
			stateDirectory, err := settings.StateDirectory()
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(stateDirectory)
			if err != nil || info.Mode().Perm() != 0711 {
				t.Fatal("setup blocked state-directory traversal", err)
			}
			if strings.Contains(output.String(), "PRIVATE KEY") || strings.Contains(output.String(), "test-code") {
				t.Fatal("setup printed a secret")
			}
			if f.conversions.Load() != 1 {
				t.Fatal("incorrect number of App creations")
			}
			if f.browserCalls.Load() != 1 {
				t.Fatal("new App setup opened another browser instead of using the callback link")
			}
			if strings.Contains(f.callbackHTML, "PRIVATE KEY") || strings.Contains(f.callbackHTML, "test-code") ||
				strings.Contains(f.callbackHTML, result.KeyPath) {
				t.Fatal("callback exposed credentials or local paths")
			}
			client := &http.Client{Timeout: time.Second}
			if response, err := client.Get(f.localURL); err == nil {
				response.Body.Close()
				t.Fatal("callback listener remained active after setup")
			}
		})
	}
}

func TestSetupWithUnquotedNumericTargetID(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	data, err := os.ReadFile(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("id: build"), []byte("id: 123"), 1)
	if err := os.WriteFile(f.options.Config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.options.ID = "123"
	var output bytes.Buffer
	result, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	config, err := settings.LoadConfig(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Target != "123" || config.Targets[0].ID != "123" ||
		config.Targets[0].InstallationID != 123 || config.Targets[0].Credential.ClientID != "Iv1.test" ||
		config.Targets[1].Credential.Type != "gh" || config.Defaults.Credential.Type != "gh" {
		t.Fatal("numeric target ID did not select the correct configuration mapping")
	}
}

func TestUnansweredConfirmationIsInterruptible(t *testing.T) {
	for _, stage := range []string{"cancellation", "deadline"} {
		t.Run(stage, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			f.options.Yes = false
			before, err := os.ReadFile(f.options.Config)
			if err != nil {
				t.Fatal(err)
			}
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			fd := reader.Fd()
			flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil {
				t.Fatal(err)
			}
			prompted := make(chan struct{})
			output := confirmationOutput{Writer: io.Discard, onPrompt: func() { close(prompted) }}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.run(ctx, f.options, reader, output)
				done <- err
			}()
			select {
			case <-prompted:
			case err := <-done:
				t.Fatal("setup failed before reaching confirmation", err)
			case <-time.After(5 * time.Second):
				cancel()
				reader.Close()
				<-done
				t.Fatal("setup did not reach confirmation")
			}
			wantError := context.DeadlineExceeded
			if stage == "cancellation" {
				cancel()
				wantError = context.Canceled
			}
			select {
			case err := <-done:
				if !errors.Is(err, wantError) {
					t.Fatal("unanswered confirmation did not preserve the context error", err)
				}
			case <-time.After(5 * time.Second):
				cancel()
				reader.Close()
				<-done
				t.Fatal("confirmation remained blocked after cancellation/deadline")
			}
			currentFlags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil || currentFlags != flags {
				t.Fatal("confirmation closed the caller's input or changed its flags", err)
			}
			after, err := os.ReadFile(f.options.Config)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("interrupted confirmation changed configuration", err)
			}
			if _, err := unix.FcntlInt(fd, unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(writer, "after\n"); err != nil {
				t.Fatal(err)
			}
			var input [6]byte
			n, err := unix.Read(int(fd), input[:])
			if err != nil || string(input[:n]) != "after\n" {
				t.Fatal("a confirmation reader consumed input after setup returned", err)
			}
			if _, err := unix.FcntlInt(fd, unix.F_SETFL, flags); err != nil {
				t.Fatal(err)
			}
			f.options.Yes = true
			result, err := f.run(t.Context(), f.options, nil, io.Discard)
			if err != nil || result.InstallationID != 123 || f.conversions.Load() != 1 {
				t.Fatal("setup lock was not released or saved App/key could not be reused", err)
			}
		})
	}
}

func TestSetupResumeAfterDecline(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Yes = false
	before, _ := os.ReadFile(f.options.Config)
	var output bytes.Buffer
	first, err := f.run(t.Context(), f.options, confirmationInput(t, "n\n"), &output)
	if err == nil || !strings.Contains(err.Error(), "not changed") {
		t.Fatal("declined configuration was applied", err)
	}
	after, _ := os.ReadFile(f.options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("declining changed configuration")
	}
	f.options.Yes = true
	second, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil || first.KeyPath != second.KeyPath || second.InstallationID != 123 ||
		f.conversions.Load() != 1 || f.ownerCalls.Load() != 1 {
		t.Fatal("resume created another App or lost metadata", second, err)
	}
	if _, err := os.Stat(second.KeyPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("resume rotated the key")
	}
}

func TestSetupDoesNotReplaceConcurrentCredential(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	browser := f.open
	stateDirectory, err := settings.StateDirectory()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(stateDirectory, "credentials", f.options.ID+".pem")
	existing := []byte("concurrently-registered-credential")
	f.open = func(ctx context.Context, address string) error {
		if err := os.WriteFile(keyPath, existing, 0600); err != nil {
			return err
		}
		return browser(ctx, address)
	}
	before, _ := os.ReadFile(f.options.Config)
	var output bytes.Buffer
	if _, err := f.run(t.Context(), f.options, nil, &output); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("setup overwrote a concurrently registered credential", err)
	}
	actual, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(actual, existing) {
		t.Fatal("existing credential was not retained", err)
	}
	after, _ := os.ReadFile(f.options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("failed setup changed the configuration")
	}
	if _, err := os.Stat(keyPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("setup rotated an unrelated credential")
	}
}

func TestSetupFailureRetainsAppAndConfiguration(t *testing.T) {
	for _, kind := range []string{"owner", "permission", "api", "concurrent-config", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			before, _ := os.ReadFile(f.options.Config)
			switch kind {
			case "owner":
				f.appOwner = "another-owner"
			case "permission":
				f.permission = "read"
			case "api":
				f.status = http.StatusForbidden
			case "concurrent-config":
				f.onInstall = func() {
					if err := os.WriteFile(f.options.Config, append(before, []byte("# concurrent edit\n")...), 0600); err != nil {
						t.Error(err)
					}
					f.installed.Store(true)
				}
			case "timeout":
				f.onInstall = func() {}
				f.timeout = 150 * time.Millisecond
			}
			var output bytes.Buffer
			result, err := f.run(t.Context(), f.options, nil, &output)
			if err == nil {
				t.Fatal("invalid setup succeeded")
			}
			if strings.Contains(err.Error()+output.String(), "response-secret-must-not-leak") ||
				strings.Contains(err.Error()+output.String(), "PRIVATE KEY") {
				t.Fatal("failure disclosed secrets")
			}
			if result.KeyPath == "" {
				t.Fatal("created key was not retained")
			}
			if _, err := os.Stat(result.MetadataPath); err != nil {
				t.Fatal("setup cannot be resumed", err)
			}
			after, _ := os.ReadFile(f.options.Config)
			if kind == "concurrent-config" {
				if !bytes.HasSuffix(after, []byte("# concurrent edit\n")) {
					t.Fatal("concurrent edit was overwritten")
				}
			} else if !bytes.Equal(before, after) {
				t.Fatal("failed setup changed configuration")
			}
		})
	}
}

func TestSetupRejectsUnsupportedOptions(t *testing.T) {
	for _, kind := range []string{"id", "listener", "scope", "host", "orphan-key"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			switch kind {
			case "id":
				f.options.ID = "../escape"
			case "listener":
				f.options.Listen = "0.0.0.0:8765"
			case "scope":
				f.options.Scope = ""
			case "host":
				f.options.Scope = "https://github.example/owner/repo"
			case "orphan-key":
				dir, _ := settings.StateDirectory()
				path := filepath.Join(dir, "credentials")
				os.MkdirAll(path, 0700)
				os.WriteFile(filepath.Join(path, "build.pem"), []byte(f.pem), 0600)
			}
			if _, err := f.run(t.Context(), f.options, nil, io.Discard); err == nil {
				t.Fatal("unsupported setup accepted")
			}
			if f.conversions.Load() != 0 || f.ownerCalls.Load() != 0 {
				t.Fatal("invalid setup contacted the remote API")
			}
		})
	}
}

func TestCanceledCallbackClosesListener(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.open = func(ctx context.Context, address string) error {
		f.localURL = address
		return nil
	}
	f.timeout = 30 * time.Millisecond
	if _, err := f.run(t.Context(), f.options, nil, io.Discard); err == nil {
		t.Fatal("callback wait succeeded without callback")
	}
	if f.conversions.Load() != 0 {
		t.Fatal("canceled callback created credentials")
	}
	if response, err := http.Get(f.localURL); err == nil {
		response.Body.Close()
		t.Fatal("canceled callback listener still responds")
	}
}
