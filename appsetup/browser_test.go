package appsetup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Songmu/ghalet/credentialstore"
)

func TestCallbackWaitsForPersistence(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.timeout = 10 * time.Second
	addresses := make(chan string, 1)
	f.open = func(_ context.Context, address string) error {
		addresses <- address
		return nil
	}
	saving, release := make(chan struct{}), make(chan struct{})
	save := f.saveKey
	f.saveKey = func(ctx context.Context, options credentialstore.Options, input io.Reader) (credentialstore.Result, error) {
		close(saving)
		<-release
		return save(ctx, options, input)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.run(ctx, f.options, nil, io.Discard)
		done <- err
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("setup did not stop")
		}
	}()
	var address string
	select {
	case address = <-addresses:
	case err := <-done:
		done <- err
		t.Fatal("setup failed before opening the browser", err)
	case <-time.After(5 * time.Second):
		t.Fatal("setup did not open the browser")
	}
	start, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	callback := start.Scheme + "://" + start.Host + "/callback?code=test-code&state=" + path.Base(start.Path)
	responses := make(chan *http.Response, 1)
	requestErrors := make(chan error, 1)
	go func() {
		response, err := http.Get(callback)
		if err != nil {
			requestErrors <- err
			return
		}
		responses <- response
	}()
	defer func() {
		close(release)
	}()
	select {
	case <-saving:
	case err := <-requestErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not attempt persistence")
	}
	select {
	case response := <-responses:
		response.Body.Close()
		t.Fatal("callback returned before saving credentials")
	default:
	}
	duplicate, err := http.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusConflict || f.conversions.Load() != 1 {
		t.Fatal("callback replay was not rejected")
	}
	// Keep this response pending past the server's ordinary five-second
	// write timeout to cover slow credential-store operations.
	time.Sleep(5100 * time.Millisecond)
	release <- struct{}{}
	var response *http.Response
	select {
	case response = <-responses:
	case err := <-requestErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not finish after saving credentials")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("callback response was lost after persistence", err)
	}
	nonce := regexp.MustCompile(`<style nonce="([^"]+)"`).FindSubmatch(body)
	if len(nonce) != 2 || !strings.Contains(response.Header.Get("Content-Security-Policy"), "'nonce-"+string(nonce[1])+"'") ||
		response.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("callback HTML did not retain its content and security headers")
	}
	directory, err := credentialstore.Directory()
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadApp(filepath.Join(directory, f.options.ID+".app.json"))
	if err != nil {
		t.Fatal("installation link was shown before resume metadata was saved", err)
	}
	if _, err := readPrivate(state.KeyPath); err != nil {
		t.Fatal("installation link was shown before the key was saved", err)
	}
	if !strings.Contains(string(body), `href="`+installationURL(state.Slug)+`"`) {
		t.Fatal("callback did not show the installation link")
	}
	f.installed.Store(true)
}

func TestCallbackFailure(t *testing.T) {
	for _, stage := range []string{"exchange", "key", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			switch stage {
			case "exchange":
				f.conversionStatus = http.StatusInternalServerError
			case "key":
				f.saveKey = func(context.Context, credentialstore.Options, io.Reader) (credentialstore.Result, error) {
					return credentialstore.Result{}, errors.New("sensitive-storage-error /private/local/path")
				}
			case "metadata":
				save := f.saveKey
				f.saveKey = func(ctx context.Context, options credentialstore.Options, input io.Reader) (credentialstore.Result, error) {
					stored, err := save(ctx, options, input)
					if err == nil {
						err = os.Mkdir(strings.TrimSuffix(stored.RuntimePath, ".pem")+".app.json", 0700)
					}
					return stored, err
				}
			}
			_, err := f.run(t.Context(), f.options, nil, io.Discard)
			if err == nil {
				t.Fatal("setup did not report callback failure in the terminal")
			}
			if f.callbackStatus != http.StatusInternalServerError ||
				!strings.Contains(f.callbackHTML, "App setup needs attention") ||
				strings.Contains(f.callbackHTML, "installations/new") ||
				strings.Contains(f.callbackHTML, "sensitive-") ||
				strings.Contains(f.callbackHTML, "/private/") ||
				strings.Contains(f.callbackHTML, "PRIVATE KEY") {
				t.Fatal("failed callback exposed details or offered installation")
			}
			if f.installed.Load() {
				t.Fatal("failed callback continued installation")
			}
		})
	}
}
