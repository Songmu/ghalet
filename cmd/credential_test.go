package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
)

func TestCredentialOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"default", "--prompt"}, ""},
		{[]string{"default", "--stdin"}, ""},
		{[]string{"--stdin", "default"}, ""},
		{[]string{"app", "--type", "github-app", "--file", "key.pem", "--client-id", "app-client"}, ""},
		{[]string{"default"}, "select exactly one"},
		{[]string{"default", "--prompt", "--stdin"}, "select exactly one"},
		{[]string{"default", "--file", "token", "--stdin"}, "select exactly one"},
		{[]string{"../escape", "--stdin"}, "credential name"},
		{[]string{"bad.name", "--stdin"}, "credential name"},
		{[]string{"bad\nname", "--stdin"}, "credential name"},
		{[]string{strings.Repeat("x", 65), "--stdin"}, "credential name"},
		{[]string{"default", "--type", "gh", "--stdin"}, "--type"},
		{[]string{"app", "--type", "github-app", "--prompt"}, "GitHub App keys require"},
		{[]string{"default", "--client-id", "client", "--stdin"}, "--client-id"},
		{[]string{"default", "--stdin", "secret-must-not-appear"}, "exactly one name"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			options, err := parseCredentialOptions(tc.args, "/selected/config.yaml", &stderr)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if options.config != "/selected/config.yaml" {
					t.Fatal("global config flag was lost")
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret-must-not-appear") {
				t.Fatal("argument contents were disclosed")
			}
		})
	}
}

func TestCredentialHelp(t *testing.T) {
	for _, args := range [][]string{
		{"credential", "--help"},
		{"credential", "set", "--help"},
		{"credential", "set", "default", "--help"},
		{"--config", "custom.yaml", "credential", "set", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(t.Context(), args, &stdout, &stderr); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(args, err)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: ghalet credential") {
			t.Fatal("help required privileges or wrote to stdout", stdout.String(), stderr.String())
		}
	}
}

func TestDefaultCredentialConfig(t *testing.T) {
	for _, prefix := range []string{
		"",
		"defaults: {}\n",
		"defaults:\n  credential:\n    type: pat\n    tokenEnv: OLD_TOKEN\n",
	} {
		data := []byte("# Keep this comment.\n" + prefix + "targets:\n  - id: build # Keep this too.\n    scope: owner/repository\n")
		updated, err := defaultCredentialConfig(data, credentialOptions{name: "new-token", kind: "pat"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(updated, []byte("# Keep this comment.")) || !bytes.Contains(updated, []byte("# Keep this too.")) {
			t.Fatal("comments lost", string(updated))
		}
		if bytes.Contains(updated, []byte("OLD_TOKEN")) || bytes.Contains(updated, []byte("tokenEnv")) {
			t.Fatal("credential was partially merged instead of replaced", string(updated))
		}
		path := filepath.Join(testDirectory(t), "config.yaml")
		if err := os.WriteFile(path, updated, 0600); err != nil {
			t.Fatal(err)
		}
		config, err := ghalet.LoadConfig(path)
		if err != nil {
			t.Fatal(err, string(updated))
		}
		if config.Targets[0].Credential.TokenFile != "/run/credentials/ghalet.service/new-token" {
			t.Fatal("generated default was not inherited", string(updated))
		}
	}
}

func TestDefaultAppCredentialConfig(t *testing.T) {
	data := []byte("defaults:\n  credential:\n    type: github-app\n    clientId: existing-client\n    privateKeyFile: old.pem\ntargets:\n  - id: build\n    scope: owner/repository\n    installationId: 123\n")
	for _, clientID := range []string{"", "new-client"} {
		updated, err := defaultCredentialConfig(data, credentialOptions{name: "app-key", kind: "github-app", clientID: clientID})
		if err != nil {
			t.Fatal(err)
		}
		want := clientID
		if want == "" {
			want = "existing-client"
		}
		if !bytes.Contains(updated, []byte(want)) || bytes.Contains(updated, []byte("old.pem")) {
			t.Fatal("App metadata was lost or partially merged", string(updated))
		}
	}
	for _, data := range []string{
		"targets: []\n",
		"defaults: not-a-mapping\ntargets: []\n",
		"unknown: value\ntargets: []\n",
		"targets: []\n---\ntargets: []\n",
	} {
		if _, err := defaultCredentialConfig([]byte(data), credentialOptions{name: "app", kind: "github-app"}); err == nil {
			t.Fatal("invalid App config accepted", data)
		}
	}
}

func TestDefaultCredentialConfigPreservesExplicitAliases(t *testing.T) {
	input := []byte("defaults:\n  credential: &shared\n    type: pat\n    tokenEnv: OLD_TOKEN # Keep credential override.\ntargets:\n  - id: build\n    scope: owner/repository\n    credential: *shared\n")
	updated, err := defaultCredentialConfig(input, credentialOptions{name: "new", kind: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(testDirectory(t), "config.yaml")
	if err := os.WriteFile(path, updated, 0600); err != nil {
		t.Fatal(err)
	}
	config, err := ghalet.LoadConfig(path)
	if err != nil {
		t.Fatal(err, string(updated))
	}
	if config.Targets[0].Credential.TokenEnv != "OLD_TOKEN" || config.Defaults.Credential.TokenEnv != "" {
		t.Fatal("editing defaults modified an explicit aliased target", string(updated))
	}
}

type credentialTestTools struct {
	calls        []string
	encryptError bool
	reloadErrors int
	promptError  bool
	onEncrypt    func()
	version      string
}

func (tools *credentialTestTools) run(ctx context.Context, tool string, args []string, input io.Reader, output io.Writer) error {
	call := tool + " " + strings.Join(args, " ")
	tools.calls = append(tools.calls, call)
	switch {
	case tool == "systemd-creds" && args[0] == "--version":
		version := tools.version
		if version == "" {
			version = "systemd 252"
		}
		_, err := io.WriteString(output, version)
		return err
	case tool == "systemd-ask-password":
		if tools.promptError {
			return errors.New("input canceled")
		}
		_, err := io.WriteString(output, "prompt-secret\n")
		return err
	case tool == "systemd-creds" && args[0] == "encrypt":
		if tools.onEncrypt != nil {
			tools.onEncrypt()
		}
		if tools.encryptError {
			return errors.New("encryption failed")
		}
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		_, err = io.WriteString(output, "encrypted:"+base64.StdEncoding.EncodeToString(data))
		return err
	case tool == "systemctl" && strings.Join(args, " ") == "daemon-reload":
		if tools.reloadErrors > 0 {
			tools.reloadErrors--
			return errors.New("reload failed")
		}
		return nil
	default:
		return errors.New("unexpected command")
	}
}

func testCredentialStore(t *testing.T) (credentialStore, credentialOptions, *credentialTestTools) {
	t.Helper()
	root := testDirectory(t)
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("# retained\ntargets:\n  - id: build\n    scope: owner/repository\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := &credentialTestTools{}
	store := credentialStore{directory: filepath.Join(root, "credentials"), dropIns: filepath.Join(root, "drop-ins"), run: tools.run}
	options := credentialOptions{name: "default", kind: "pat", stdin: true, config: config}
	return store, options, tools
}

func TestCredentialRegistrationAndRotation(t *testing.T) {
	store, options, tools := testCredentialStore(t)
	var out bytes.Buffer
	if err := store.set(t.Context(), options, strings.NewReader("first-secret\n"), &out); err != nil {
		t.Fatal(err)
	}
	cipherPath := filepath.Join(store.directory, "default.cred")
	cipher, err := os.ReadFile(cipherPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cipherPath, options.config} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("unexpected file permissions", path, info, err)
		}
	}
	dropIn, err := os.ReadFile(filepath.Join(store.dropIns, "credential-default.conf"))
	if err != nil || string(dropIn) != "[Service]\nLoadCredentialEncrypted=default:"+cipherPath+"\n" {
		t.Fatal("incorrect drop-in", string(dropIn), err)
	}
	if strings.Contains(out.String(), "first-secret") || bytes.Contains(cipher, []byte("first-secret")) {
		t.Fatal("plaintext leaked")
	}
	if err := store.set(t.Context(), options, strings.NewReader("second-secret"), &out); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(cipherPath + ".previous")
	if err != nil || !bytes.Equal(backup, cipher) {
		t.Fatal("previous ciphertext was not retained", err)
	}
	if !strings.Contains(out.String(), "Drain successfully") {
		t.Fatal("rotation instructions missing")
	}
	if err := store.set(t.Context(), options, strings.NewReader("third-secret"), &out); err == nil {
		t.Fatal("overwrote the existing rollback backup")
	}
	if strings.Contains(strings.Join(tools.calls, "\n"), "restart") || strings.Contains(strings.Join(tools.calls, "\n"), "third-secret") {
		t.Fatal("restarted the service or passed a secret in argv")
	}
}

func TestCredentialRegistrationFailureIsNonDestructive(t *testing.T) {
	for _, stage := range []string{"encryption", "reload", "empty", "multiline", "oversized", "canceled", "old-systemd", "changed-config", "bad-config"} {
		t.Run(stage, func(t *testing.T) {
			store, options, tools := testCredentialStore(t)
			var out bytes.Buffer
			if err := store.set(t.Context(), options, strings.NewReader("old-secret"), &out); err != nil {
				t.Fatal(err)
			}
			cipherPath := filepath.Join(store.directory, "default.cred")
			oldCipher, _ := os.ReadFile(cipherPath)
			oldConfig, _ := os.ReadFile(options.config)
			secret := "new-secret"
			switch stage {
			case "encryption":
				tools.encryptError = true
			case "reload":
				tools.reloadErrors = 1
			case "empty":
				secret = "  \n"
			case "multiline":
				secret = "one\ntwo"
			case "oversized":
				secret = strings.Repeat("x", maxCredentialSize+1)
			case "canceled":
				options.stdin, options.prompt, tools.promptError = false, true, true
			case "old-systemd":
				tools.version = "systemd 249"
			case "changed-config":
				tools.onEncrypt = func() {
					if err := os.WriteFile(options.config, []byte("# concurrently edited\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "bad-config":
				if err := os.WriteFile(options.config, []byte("unknown: value\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			if err := store.set(t.Context(), options, strings.NewReader(secret), &out); err == nil {
				t.Fatal("failure was not reported")
			}
			current, _ := os.ReadFile(cipherPath)
			if !bytes.Equal(current, oldCipher) {
				t.Fatal("old ciphertext was lost")
			}
			if _, err := os.Stat(cipherPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed update left a rollback backup", err)
			}
			if stage != "changed-config" && stage != "bad-config" {
				current, _ = os.ReadFile(options.config)
				if !bytes.Equal(current, oldConfig) {
					t.Fatal("old config was lost", string(current))
				}
			}
			if stage == "changed-config" {
				current, _ = os.ReadFile(options.config)
				if string(current) != "# concurrently edited\n" {
					t.Fatal("overwrote concurrent edits")
				}
			}
			if out.Len() != 0 {
				t.Fatal("reported success after failure", out.String())
			}
		})
	}
}

func TestCredentialNonDefaultAndAppInput(t *testing.T) {
	store, options, _ := testCredentialStore(t)
	options.name, options.kind, options.noDefault = "app-key", "github-app", true
	if err := os.Remove(options.config); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	options.file, options.stdin = filepath.Join(testDirectory(t), "app.pem"), false
	if err := os.WriteFile(options.file, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := store.set(t.Context(), options, nil, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(options.config); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--no-default wrote configuration", err)
	}
	if !strings.Contains(out.String(), "privateKeyFile: /run/credentials/ghalet.service/app-key") {
		t.Fatal("App reference missing", out.String())
	}
	if strings.Contains(out.String(), "PRIVATE KEY") {
		t.Fatal("App key leaked")
	}
	if err := os.Chmod(options.file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readSecret(t.Context(), options, nil); err == nil {
		t.Fatal("accepted a world-readable input key")
	}
	options.file = ""
	for _, invalid := range []string{"not a key", string(data) + string(data)} {
		if _, err := store.readSecret(t.Context(), options, strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}

func TestCredentialCustomizedDropInIsNotOverwritten(t *testing.T) {
	store, options, _ := testCredentialStore(t)
	if err := os.MkdirAll(store.dropIns, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.dropIns, "credential-default.conf")
	original := []byte("[Service]\n# Local configuration\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	err := store.set(t.Context(), options, strings.NewReader("secret"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "customized systemd drop-in") {
		t.Fatal("customized drop-in was accepted", err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("customized drop-in was changed")
	}
}

func TestCredentialPathsRejectSymlinks(t *testing.T) {
	for _, target := range []string{"cipher", "drop-in", "config", "directory"} {
		t.Run(target, func(t *testing.T) {
			store, options, _ := testCredentialStore(t)
			if err := os.MkdirAll(store.directory, 0700); err != nil {
				t.Fatal(err)
			}

			if err := os.MkdirAll(store.dropIns, 0755); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(testDirectory(t), "protected")
			if err := os.WriteFile(dest, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			path := options.config
			switch target {
			case "cipher":
				path = filepath.Join(store.directory, "default.cred")
			case "drop-in":
				path = filepath.Join(store.dropIns, "credential-default.conf")
			case "config":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(store.directory); err != nil {
					t.Fatal(err)
				}
				path = store.directory
			}
			if err := os.Symlink(dest, path); err != nil {
				t.Fatal(err)
			}
			if err := store.set(t.Context(), options, strings.NewReader("secret"), io.Discard); err == nil {
				t.Fatal("symlink was accepted")
			}
			data, _ := os.ReadFile(dest)
			if string(data) != "untouched" {
				t.Fatal("symlink target was modified")
			}
		})
	}
}
