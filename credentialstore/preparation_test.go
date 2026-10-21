package credentialstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialPreparationRejectsBeforeInput(t *testing.T) {
	for _, backend := range []string{"file", "systemd"} {
		for _, failure := range []string{"no-replace", "previous", "missing-config"} {
			t.Run(backend+"/"+failure, func(t *testing.T) {
				store, options, _ := testCredentialStore(t)
				set := store.set
				extension := ".cred"
				if backend == "file" {
					extension = ".pat"
					set = func(ctx context.Context, options Options, input io.Reader) (Result, error) {
						return setFile(ctx, store.directory, options, input)
					}
				}
				result, err := set(t.Context(), options, strings.NewReader("old-secret"))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(store.directory, options.Name+extension)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var want string
				switch failure {
				case "no-replace":
					options.NoReplace = true
					want = "credential already exists at " + path + "; existing credential unchanged"
				case "previous":
					if _, err := set(t.Context(), options, strings.NewReader("next-secret")); err != nil {
						t.Fatal(err)
					}
					before, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					want = "previous credential backup already exists at " + path + ".previous; finish or roll back the previous rotation"
					if backend == "systemd" {
						want += " before updating again"
					}
				case "missing-config":
					if err := os.Remove(options.Config); err != nil {
						t.Fatal(err)
					}
					want = "configuration does not exist; create it first or use --no-default"
					if backend == "systemd" {
						want = "configuration " + options.Config + " does not exist; copy the package example first, or use --no-default"
					}
				}
				input := strings.NewReader("replacement-secret")
				if _, err := set(t.Context(), options, input); err == nil || err.Error() != want || input.Len() != len("replacement-secret") {
					t.Fatal("preparation failure changed diagnostics or consumed input", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("preparation failure changed credential", result, err)
				}
			})
		}
	}
}
