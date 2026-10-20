package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdCredentialEncryption(t *testing.T) {
	if os.Getenv("GHALET_TEST_SYSTEMD_CREDENTIALS") != "1" || os.Geteuid() != 0 {
		t.Skip("set GHALET_TEST_SYSTEMD_CREDENTIALS=1 as root on a disposable Linux host")
	}
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		t.Fatal(err)
	}
	store, options, _ := testCredentialStore(t)
	reloads := 0
	store.run = func(ctx context.Context, tool string, args []string, input io.Reader, output io.Writer) error {
		if tool == "systemctl" {
			if strings.Join(args, " ") != "daemon-reload" {
				t.Fatal("attempted service operation", args)
			}
			reloads++
			return nil
		}
		return runCredentialCommand(ctx, tool, args, input, output)
	}
	var out bytes.Buffer
	cipherPath := filepath.Join(store.directory, "default.cred")
	for _, secret := range []string{"first-test-pat", "rotated-test-pat"} {
		if err := store.set(t.Context(), options, strings.NewReader(secret), &out); err != nil {
			t.Fatal(err)
		}
		var plain bytes.Buffer
		if err := runCredentialCommand(t.Context(), "systemd-creds", []string{"decrypt", "--name=default", cipherPath, "-"}, nil, &plain); err != nil {
			t.Fatal(err)
		}
		if plain.String() != secret {
			t.Fatal("credential did not round-trip")
		}
		data, err := os.ReadFile(cipherPath)
		if err != nil || bytes.Contains(data, []byte(secret)) || strings.Contains(out.String(), secret) {
			t.Fatal("plaintext was saved or printed", err)
		}
	}
	if reloads != 2 {
		t.Fatal("unexpected number of reloads", reloads)
	}
	var previous bytes.Buffer
	if err := runCredentialCommand(t.Context(), "systemd-creds", []string{"decrypt", "--name=default", cipherPath + ".previous", "-"}, nil, &previous); err != nil {
		t.Fatal(err)
	}
	if previous.String() != "first-test-pat" {
		t.Fatal("previous credential did not round-trip")
	}
	if err := runCredentialCommand(t.Context(), "systemd-creds", []string{"decrypt", "--name=different", cipherPath, "-"}, nil, io.Discard); err == nil {
		t.Fatal("credential was accepted under a different name")
	}
}
