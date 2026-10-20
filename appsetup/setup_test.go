package appsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	"testing"
	"time"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/golang-jwt/jwt/v5"
)

type fixture struct {
	service
	options     Options
	key         *rsa.PrivateKey
	pem         string
	ownerType   string
	scope       string
	appOwner    string
	permission  string
	installed   atomic.Bool
	conversions atomic.Int32
	ownerCalls  atomic.Int32
	status      int
	localURL    string
	onInstall   func()
	onLookup    func()
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
		options: Options{Name: "test-app", Config: filepath.Join(root, "config.yaml"), Yes: true},
	}
	data := "# keep this comment\ndefaults:\n  credential: &shared\n    type: gh\ntargets:\n  - id: build\n    scope: " + scope + "\n  - id: other\n    scope: owner/other\n    credential: *shared\n"
	if err := os.WriteFile(f.options.Config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	f.options.Target = "build"
	server := httptest.NewServer(http.HandlerFunc(f.apiHandler(t)))
	t.Cleanup(server.Close)
	f.service = service{
		api:  apiClient{base: server.URL, http: server.Client()},
		open: f.browser(t), interval: time.Millisecond, timeout: 2 * time.Second,
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

type confirmationReader func([]byte) (int, error)

func (read confirmationReader) Read(data []byte) (int, error) { return read(data) }

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
			keyPath := filepath.Join(directory, f.options.Name+".pem")
			rotate := func() {
				_, err := credentialstore.SetFile(t.Context(), credentialstore.Options{
					Name: f.options.Name, Type: "github-app", Stdin: true, NoDefault: true,
				}, bytes.NewReader(replacement))
				if err != nil {
					t.Error("could not perform concurrent key replacement", err)
				}
			}
			var input io.Reader
			if stage == "polling" {
				f.onLookup = rotate
			} else {
				f.options.Yes = false
				input = confirmationReader(func(data []byte) (int, error) {
					rotate()
					return copy(data, "yes\n"), io.EOF
				})
			}
			var output bytes.Buffer
			_, err = f.run(t.Context(), f.options, input, &output)
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
		finished.Body.Close()
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
			config, err := ghalet.LoadConfig(f.options.Config)
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
			stateDirectory, err := ghalet.StateDirectory()
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
			client := &http.Client{Timeout: time.Second}
			if response, err := client.Get(f.localURL); err == nil {
				response.Body.Close()
				t.Fatal("callback listener remained active after setup")
			}
		})
	}
}

func TestSetupResumeAfterDecline(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Yes = false
	before, _ := os.ReadFile(f.options.Config)
	var output bytes.Buffer
	first, err := f.run(t.Context(), f.options, strings.NewReader("n\n"), &output)
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
	stateDirectory, err := ghalet.StateDirectory()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(stateDirectory, "credentials", f.options.Name+".pem")
	existing := []byte("concurrently-registered-credential")
	f.open = func(ctx context.Context, address string) error {
		if err := browser(ctx, address); err != nil {
			return err
		}
		return os.WriteFile(keyPath, existing, 0600)
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
	for _, kind := range []string{"name", "listener", "target", "host", "orphan-key"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			switch kind {
			case "name":
				f.options.Name = "../escape"
			case "listener":
				f.options.Listen = "0.0.0.0:8765"
			case "target":
				f.options.Target = ""
			case "host":
				os.WriteFile(f.options.Config, []byte("targets:\n  - id: build\n    scope: https://github.example/owner/repo\n"), 0600)
			case "orphan-key":
				dir, _ := ghalet.StateDirectory()
				path := filepath.Join(dir, "credentials")
				os.MkdirAll(path, 0700)
				os.WriteFile(filepath.Join(path, "test-app.pem"), []byte(f.pem), 0600)
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
