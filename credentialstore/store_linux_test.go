package credentialstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
	cipherPath := filepath.Join(store.directory, "default.cred")
	for _, secret := range []string{"first-test-pat", "rotated-test-pat"} {
		if _, err := store.set(t.Context(), options, strings.NewReader(secret)); err != nil {
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
		if err != nil || bytes.Contains(data, []byte(secret)) {
			t.Fatal("plaintext was saved", err)
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

func TestSystemdAppKeyEncryption(t *testing.T) {
	if os.Getenv("GHALET_TEST_SYSTEMD_CREDENTIALS") != "1" || os.Geteuid() != 0 {
		t.Skip("set GHALET_TEST_SYSTEMD_CREDENTIALS=1 as root on a disposable Linux host")
	}
	store, options, _ := testCredentialStore(t)
	options.Name, options.Type, options.NoDefault, options.NoReplace = "app-key", "github-app", true, true
	store.run = func(ctx context.Context, tool string, args []string, input io.Reader, output io.Writer) error {
		if tool == "systemctl" && strings.Join(args, " ") == "daemon-reload" {
			return nil
		}
		return runCredentialCommand(ctx, tool, args, input, output)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	result, err := store.saveAppKey(t.Context(), options, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := store.read(t.Context(), options.Name)
	if err != nil || !bytes.Equal(plain, bytes.TrimSpace(data)) {
		t.Fatal("App key did not round-trip through systemd-creds", err)
	}
	clear(plain)
	lock, err := LockDirectory(store.directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.provisionAppKey(t.Context(), options.Name); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	lock.Close()
	cipher, err := os.ReadFile(result.EncryptedPath)
	if err != nil || bytes.Contains(cipher, []byte("PRIVATE KEY")) {
		t.Fatal("plaintext App key was saved", err)
	}
	if _, err := store.saveAppKey(t.Context(), options, bytes.NewReader(data)); err == nil {
		t.Fatal("setup replaced an existing encrypted App key")
	}
}
