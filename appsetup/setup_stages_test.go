package appsetup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet/credentialstore"
)

func TestCreateRejectsNonLoopbackListener(t *testing.T) {
	for _, listen := range []string{"", "localhost:0", "0.0.0.0:0", "[::]:0", "127.0.0.1", "invalid"} {
		t.Run(listen, func(t *testing.T) {
			err := (service{}).create(t.Context(), Options{Listen: listen}, "owner/repo", "User", "administration", io.Discard, func(app) error {
				t.Fatal("invalid listener reached credential persistence")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "--listen must use a loopback IP address and port") {
				t.Fatal("create did not validate its listener boundary", err)
			}
		})
	}
}

func TestSetupPreservesPartialResult(t *testing.T) {
	for _, stage := range []string{"preflight", "conversion", "save", "owner", "installation", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			switch stage {
			case "preflight":
				f.options.Listen = "0.0.0.0:0"
			case "conversion":
				f.conversionStatus = http.StatusBadRequest
			case "save":
				f.saveKey = func(context.Context, credentialstore.Options, io.Reader) (credentialstore.Result, error) {
					return credentialstore.Result{}, errors.New("cannot save key")
				}
			case "owner":
				f.appOwner = "another-owner"
			case "installation":
				f.status = http.StatusForbidden
			case "confirmation":
				f.options.Yes = false
			}
			directory, err := credentialstore.Directory()
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.run(t.Context(), f.options, confirmationInput(t, "n\n"), io.Discard)
			if err == nil {
				t.Fatal("setup unexpectedly succeeded")
			}
			want := Result{MetadataPath: filepath.Join(directory, "build.app.json"), Target: "build"}
			switch stage {
			case "preflight":
				want = Result{}
			case "owner", "installation", "confirmation":
				want.ClientID = "Iv1.test"
				want.KeyPath = filepath.Join(directory, "build.pem")
			}
			if stage == "confirmation" {
				want.InstallationID = 123
			}
			if result != want {
				t.Fatalf("partial result = %#v; want %#v", result, want)
			}
		})
	}
}
