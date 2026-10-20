package appsetup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type cancelAfterConversion struct {
	http.RoundTripper
	cancel context.CancelFunc
}

func (transport cancelAfterConversion) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.RoundTripper.RoundTrip(request)
	if err == nil && strings.HasPrefix(request.URL.Path, "/app-manifests/") {
		response.Body = cancelAfterEOF{ReadCloser: response.Body, cancel: transport.cancel}
	}
	return response, err
}

type cancelAfterEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body cancelAfterEOF) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		body.cancel()
	}
	return n, err
}

func TestCancellationAfterConversionRetainsCredentials(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	before, err := os.ReadFile(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transport := f.api.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	f.api.http.Transport = cancelAfterConversion{RoundTripper: transport, cancel: cancel}
	var output bytes.Buffer
	first, err := f.run(ctx, f.options, nil, &output)
	if !errors.Is(err, context.Canceled) || f.conversions.Load() != 1 {
		t.Fatal("setup was not canceled after the one-time code conversion", err)
	}
	key, err := readPrivate(first.KeyPath)
	if err != nil || !bytes.Equal(bytes.TrimSpace(key), bytes.TrimSpace([]byte(f.pem))) {
		t.Fatal("cancellation after conversion lost the App key", err)
	}
	state, err := loadApp(first.MetadataPath)
	if err != nil || state.ID != 77 || state.ClientID != "Iv1.test" || state.KeyPath != first.KeyPath {
		t.Fatal("cancellation after conversion lost resumption metadata", err)
	}
	for _, path := range []string{first.KeyPath, first.MetadataPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("retained credentials are not private", err)
		}
	}
	after, err := os.ReadFile(f.options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled setup changed configuration", err)
	}
	if f.installed.Load() {
		t.Fatal("canceled setup continued to installation")
	}
	second, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil || second.InstallationID != 123 || second.KeyPath != first.KeyPath ||
		f.conversions.Load() != 1 || f.ownerCalls.Load() != 1 {
		t.Fatal("resume did not reuse the saved App and key", err)
	}
	if _, err := os.Stat(first.KeyPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resuming unexpectedly rotated the key", err)
	}
}
