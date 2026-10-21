package credentialstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetRejectsInvalidOptionsBeforeInput(t *testing.T) {
	input := strings.NewReader("must-not-be-read")
	result, err := Set(t.Context(), Options{Name: "../escape", Type: "pat", Stdin: true}, input)
	if err == nil || !strings.Contains(err.Error(), "credential name") {
		t.Fatal("invalid name was not rejected before platform checks", err)
	}
	if result != (Result{}) || input.Len() != len("must-not-be-read") {
		t.Fatal("invalid registration returned a result or consumed secret input", result)
	}
}

func TestManagedDefaultsPreserveStaticConfiguration(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			store, options, _ := testCredentialStore(t)
			data := []byte("# Keep this comment.\nmanagedFile: state/managed.yaml\ndefaults:\n  credential: &shared\n    type: pat\n    tokenEnv: STATIC_PAT\ntargets:\n  - id: build\n    scope: owner/repository\n    credential: *shared\n")
			if err := os.WriteFile(options.Config, data, 0444); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(options.Config, 0444); err != nil {
				t.Fatal(err)
			}
			var result Result
			var err error
			if encrypted {
				result, err = store.set(t.Context(), options, strings.NewReader("synthetic-secret"))
			} else {
				result, err = setFile(t.Context(), store.directory, options, strings.NewReader("synthetic-secret"))
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err := os.ReadFile(options.Config)
			if err != nil || !bytes.Equal(current, data) {
				t.Fatal("static configuration was rewritten", err)
			}
			config, err := settings.LoadConfig(options.Config)
			if err != nil || config.Defaults.Credential.TokenEnv != "STATIC_PAT" || config.Targets[0].Credential.TokenEnv != "STATIC_PAT" {
				t.Fatal("managed defaults replaced handwritten authentication", err)
			}
			managed, _, err := settings.ReadManaged(result.ManagedPath)
			if err != nil || managed.Defaults.Credential.TokenFile != result.RuntimePath {
				t.Fatal("generated default was not saved separately", err)
			}
		})
	}
}

func TestManagedAppDefaultClientID(t *testing.T) {
	_, options, _ := testCredentialStore(t)
	options.Type = "github-app"
	if _, err := prepareCredentialConfig(options, "/runtime/app.pem", "missing"); err == nil {
		t.Fatal("App default without a client ID accepted")
	}
	options.ClientID = "existing"
	prepared, err := prepareCredentialConfig(options, "/runtime/app.pem", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.WriteManaged(prepared.original.path, nil, prepared.data); err != nil {
		t.Fatal(err)
	}
	options.ClientID = ""
	prepared, err = prepareCredentialConfig(options, "/runtime/new.pem", "missing")
	if err != nil {
		t.Fatal(err)
	}
	managed, err := settings.ParseManaged(prepared.data)
	if err != nil || managed.Defaults.Credential.ClientID != "existing" ||
		managed.Defaults.Credential.PrivateKeyFile != "/runtime/new.pem" {
		t.Fatal("App client ID was not retained as a complete credential", err)
	}
	static := []byte("managedFile: managed.yaml\ndefaults:\n  credential:\n    type: github-app\n    clientId: static\n    privateKeyFile: static.pem\ntargets: []\n")
	if err := os.WriteFile(options.Config, static, 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err = prepareCredentialConfig(options, "/runtime/new.pem", "missing")
	if err != nil {
		t.Fatal(err)
	}
	managed, err = settings.ParseManaged(prepared.data)
	if err != nil || managed.Defaults.Credential.ClientID != "static" {
		t.Fatal("static App identity did not take precedence", err)
	}
}

type credentialTestTools struct {
	calls        []string
	encryptError bool
	reloadErrors int
	promptError  bool
	onEncrypt    func()
	onReload     func()
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
	case tool == "systemd-creds" && args[0] == "decrypt":
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		plain, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(data), "encrypted:"))
		if err != nil {
			return err
		}
		_, err = output.Write(plain)
		return err
	case tool == "systemctl" && strings.Join(args, " ") == "daemon-reload":
		if tools.onReload != nil {
			tools.onReload()
		}
		if tools.reloadErrors > 0 {
			tools.reloadErrors--
			return errors.New("reload failed")
		}
		return nil
	default:
		return errors.New("unexpected command")
	}
}

func TestEncryptedAppKeyReadAndNoReplace(t *testing.T) {
	store, options, tools := testCredentialStore(t)
	options.Name, options.Type, options.NoDefault, options.NoReplace = "app-key", "github-app", true, true
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	result, err := store.set(t.Context(), options, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := os.ReadFile(result.EncryptedPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := store.read(t.Context(), options.Name)
	if err != nil || !bytes.Equal(plain, bytes.TrimSpace(data)) {
		t.Fatal("encrypted App key did not round-trip", err)
	}
	clear(plain)
	input := bytes.NewReader(data)
	if _, err := store.set(t.Context(), options, input); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("setup replaced an existing encrypted credential", err)
	}
	current, err := os.ReadFile(result.EncryptedPath)
	if err != nil || !bytes.Equal(current, cipher) || input.Len() != len(data) {
		t.Fatal("exclusive registration consumed input or changed the key", err)
	}
	if _, err := os.Stat(result.EncryptedPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exclusive registration rotated the credential", err)
	}
	if !strings.Contains(strings.Join(tools.calls, "\n"), "decrypt --name=app-key - -") {
		t.Fatal("decryption did not bind the expected credential name")
	}
	toolsRun := store.run
	store.run = func(ctx context.Context, tool string, args []string, input io.Reader, output io.Writer) error {
		if tool == "systemd-creds" && args[0] == "decrypt" {
			io.WriteString(output, "PRIVATE KEY must-not-leak")
			return errors.New("decryption failed")
		}
		return toolsRun(ctx, tool, args, input, output)
	}
	if plain, err := store.read(t.Context(), options.Name); err == nil || plain != nil ||
		strings.Contains(err.Error(), "must-not-leak") {
		t.Fatal("failed decryption exposed plaintext", err)
	}
	store.run = toolsRun
	if err := os.Remove(result.DropInPath); err != nil {
		t.Fatal(err)
	}
	plain, err = store.read(t.Context(), options.Name)
	if err != nil || !bytes.Equal(plain, bytes.TrimSpace(data)) {
		t.Fatal("key recovery depended on the service drop-in", err)
	}
	clear(plain)
}

func TestAppKeyProvisioningFailureKeepsSavedCredential(t *testing.T) {
	for _, failure := range []string{"reload", "custom-drop-in", "drop-in-directory"} {
		t.Run(failure, func(t *testing.T) {
			store, options, tools := testCredentialStore(t)
			options.Name, options.Type, options.NoDefault, options.NoReplace = "app-key", "github-app", true, true
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
			result, err := store.saveAppKey(t.Context(), options, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := os.ReadFile(result.EncryptedPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(tools.calls, "\n"), "systemctl") {
				t.Fatal("saving the App key provisioned the service")
			}
			dropInPath := filepath.Join(store.dropIns, "credential-app-key.conf")
			switch failure {
			case "reload":
				tools.reloadErrors = 1
			case "custom-drop-in":
				if err := os.MkdirAll(store.dropIns, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dropInPath, []byte("# customized\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "drop-in-directory":
				if err := os.WriteFile(store.dropIns, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := LockDirectory(store.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := store.provisionAppKey(t.Context(), options.Name); err == nil {
				t.Fatal("provisioning failure was not reported")
			}
			current, err := os.ReadFile(result.EncryptedPath)
			if err != nil || !bytes.Equal(current, cipher) {
				t.Fatal("provisioning failure removed or replaced the saved key", err)
			}
			plain, err := store.read(t.Context(), options.Name)
			if err != nil || !bytes.Equal(plain, bytes.TrimSpace(data)) {
				t.Fatal("saved key cannot be recovered after provisioning failure", err)
			}
			clear(plain)
			if failure == "custom-drop-in" {
				if err := os.Remove(dropInPath); err != nil {
					t.Fatal(err)
				}
			} else if failure == "drop-in-directory" {
				if err := os.Remove(store.dropIns); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.provisionAppKey(t.Context(), options.Name); err != nil {
				t.Fatal("service provisioning could not be retried", err)
			}
			if strings.Count(strings.Join(tools.calls, "\n"), "systemd-creds encrypt") != 1 {
				t.Fatal("retry encrypted or replaced the key again")
			}
			if _, err := os.Stat(result.EncryptedPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("provisioning rotated the saved key", err)
			}
		})
	}
}

func testCredentialStore(t *testing.T) (credentialStore, Options, *credentialTestTools) {
	t.Helper()
	root := testDirectory(t)
	t.Setenv("STATE_DIRECTORY", filepath.Join(root, "state"))
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("# retained\nmanagedFile: managed.yaml\ntargets:\n  - id: build\n    scope: owner/repository\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := &credentialTestTools{}
	store, err := newSystemdStore(filepath.Join(root, "credentials"), filepath.Join(root, "drop-ins"))
	if err != nil {
		t.Fatal(err)
	}
	store.run = tools.run
	options := Options{Name: "default", Type: "pat", Stdin: true, Config: config}
	return store, options, tools
}

func TestSystemdCredentialSourcesSurviveLayoutMigration(t *testing.T) {
	for _, operation := range []string{"registration", "rotation", "provision retry"} {
		t.Run(operation, func(t *testing.T) {
			store, options, tools := testCredentialStore(t)
			options.NoDefault = true
			secret := []byte("first-secret")
			if operation == "provision retry" {
				options.Type = "github-app"
				key, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				secret = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
			}
			var original []byte
			if operation != "registration" {
				result, err := store.set(t.Context(), options, bytes.NewReader(secret))
				if err != nil {
					t.Fatal(err)
				}
				original, err = os.ReadFile(result.EncryptedPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Dir(store.directory)
			moved := filepath.Join(testDirectory(t), "ghalet-%H")
			if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, root); err != nil {
				t.Fatal(err)
			}
			migrated, err := newSystemdStore(store.sourceDirectory, store.dropIns)
			if err != nil {
				t.Fatal(err)
			}
			migrated.run = tools.run
			if migrated.directory != filepath.Join(moved, "credentials") || migrated.dropIns != filepath.Join(moved, "drop-ins") {
				t.Fatal("filesystem locations were not canonicalized", migrated)
			}
			if operation == "provision retry" {
				if err := migrated.provisionAppKey(t.Context(), options.Name); err != nil {
					t.Fatal("existing unit source blocked setup retry", err)
				}
			} else if _, err := migrated.set(t.Context(), options, strings.NewReader("next-secret")); err != nil {
				t.Fatal("credential registration or rotation failed after migration", err)
			}
			dropIn, err := os.ReadFile(filepath.Join(migrated.dropIns, "credential-"+options.Name+".conf"))
			want := "[Service]\nLoadCredentialEncrypted=" + options.Name + ":" + filepath.Join(store.sourceDirectory, options.Name+".cred") + "\n"
			if err != nil || string(dropIn) != want || bytes.Contains(dropIn, []byte("%H")) {
				t.Fatal("unit source changed or exposed a target-path specifier", string(dropIn), err)
			}
			if operation == "rotation" {
				previous, err := os.ReadFile(filepath.Join(migrated.directory, options.Name+".cred.previous"))
				if err != nil || !bytes.Equal(previous, original) {
					t.Fatal("rotation did not preserve the original credential", err)
				}
			}
		})
	}
}

func TestCredentialPromptReceivesCallerInput(t *testing.T) {
	store, options, tools := testCredentialStore(t)
	options.Prompt, options.Stdin = true, false
	input := strings.NewReader("not-the-prompt-response")
	store.run = func(ctx context.Context, tool string, args []string, reader io.Reader, output io.Writer) error {
		if tool == "systemd-ask-password" && reader != input {
			return errors.New("password prompt did not receive the caller's input")
		}
		return tools.run(ctx, tool, args, reader, output)
	}
	secret, err := store.readSecret(t.Context(), options, input)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	if string(secret) != "prompt-secret" {
		t.Fatal("credential did not come from the password prompt")
	}
}

func TestCredentialRegistrationAndRotation(t *testing.T) {
	store, options, tools := testCredentialStore(t)
	result, err := store.set(t.Context(), options, strings.NewReader("first-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	cipherPath := filepath.Join(store.directory, "default.cred")
	if result != (Result{
		EncryptedPath: cipherPath,
		DropInPath:    filepath.Join(store.dropIns, "credential-default.conf"),
		RuntimePath:   "/run/credentials/ghalet.service/default",
		ManagedPath:   filepath.Join(filepath.Dir(options.Config), "managed.yaml"),
	}) {
		t.Fatal("incorrect registration result", result)
	}
	cipher, err := os.ReadFile(cipherPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cipherPath, options.Config} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("unexpected file permissions", path, info, err)
		}
	}
	dropIn, err := os.ReadFile(filepath.Join(store.dropIns, "credential-default.conf"))
	if err != nil || string(dropIn) != "[Service]\nLoadCredentialEncrypted=default:"+cipherPath+"\n" {
		t.Fatal("incorrect drop-in", string(dropIn), err)
	}
	if strings.Contains(fmt.Sprint(result), "first-secret") || bytes.Contains(cipher, []byte("first-secret")) {
		t.Fatal("plaintext leaked")
	}
	result, err = store.set(t.Context(), options, strings.NewReader("second-secret"))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(cipherPath + ".previous")
	if err != nil || !bytes.Equal(backup, cipher) {
		t.Fatal("previous ciphertext was not retained", err)
	}
	if result.PreviousPath != cipherPath+".previous" {
		t.Fatal("rotation recovery path missing", result)
	}
	if _, err := store.set(t.Context(), options, strings.NewReader("third-secret")); err == nil {
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
			if _, err := store.set(t.Context(), options, strings.NewReader("old-secret")); err != nil {
				t.Fatal(err)
			}
			cipherPath := filepath.Join(store.directory, "default.cred")
			oldCipher, _ := os.ReadFile(cipherPath)
			oldConfig, _ := os.ReadFile(options.Config)
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
				options.Stdin, options.Prompt, tools.promptError = false, true, true
			case "old-systemd":
				tools.version = "systemd 249"
			case "changed-config":
				tools.onEncrypt = func() {
					if err := os.WriteFile(options.Config, []byte("# concurrently edited\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "bad-config":
				if err := os.WriteFile(options.Config, []byte("unknown: value\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := store.set(t.Context(), options, strings.NewReader(secret))
			if err == nil {
				t.Fatal("failure was not reported")
			}
			if result != (Result{}) {
				t.Fatal("returned a success result after failure", result)
			}
			current, _ := os.ReadFile(cipherPath)
			if !bytes.Equal(current, oldCipher) {
				t.Fatal("old ciphertext was lost")
			}
			if _, err := os.Stat(cipherPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed update left a rollback backup", err)
			}
			if stage != "changed-config" && stage != "bad-config" {
				current, _ = os.ReadFile(options.Config)
				if !bytes.Equal(current, oldConfig) {
					t.Fatal("old config was lost", string(current))
				}
			}
			if stage == "changed-config" {
				current, _ = os.ReadFile(options.Config)
				if string(current) != "# concurrently edited\n" {
					t.Fatal("overwrote concurrent edits")
				}
			}
		})
	}
}

func TestCredentialNonDefaultAndAppInput(t *testing.T) {
	store, options, _ := testCredentialStore(t)
	options.Name, options.Type, options.NoDefault = "app-key", "github-app", true
	if err := os.Remove(options.Config); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	options.File, options.Stdin = filepath.Join(testDirectory(t), "app.pem"), false
	if err := os.WriteFile(options.File, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := store.set(t.Context(), options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(options.Config); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--no-default wrote configuration", err)
	}
	if result.RuntimePath != "/run/credentials/ghalet.service/app-key" {
		t.Fatal("App reference missing", result)
	}
	if strings.Contains(fmt.Sprint(result), "PRIVATE KEY") {
		t.Fatal("App key leaked")
	}
	if err := os.Chmod(options.File, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readSecret(t.Context(), options, nil); err == nil {
		t.Fatal("accepted a world-readable input key")
	}
	options.File = ""
	for _, invalid := range []string{"not a key", string(data) + string(data)} {
		if _, err := store.readSecret(t.Context(), options, strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}

func TestCredentialRollbackPreservesBackupOnRestoreFailure(t *testing.T) {
	store, options, tools := testCredentialStore(t)
	if _, err := store.set(t.Context(), options, strings.NewReader("old-secret")); err != nil {
		t.Fatal(err)
	}
	cipherPath := filepath.Join(store.directory, "default.cred")
	oldCipher, err := os.ReadFile(cipherPath)
	if err != nil {
		t.Fatal(err)
	}
	oldConfig, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	tools.reloadErrors = 1
	tools.onReload = func() {
		tools.onReload = nil
		// Block ciphertext restoration without preventing backup removal.
		if err := os.Rename(cipherPath, cipherPath+".new"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cipherPath, 0700); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.set(t.Context(), options, strings.NewReader("new-secret"))
	previousPath := cipherPath + ".previous"
	if err == nil || !strings.Contains(err.Error(), "restore "+cipherPath+":") ||
		!strings.Contains(err.Error(), "previous encrypted credential retained at "+previousPath) {
		t.Fatal("restore failure and recovery path were not reported", err)
	}
	backup, err := os.ReadFile(previousPath)
	if err != nil || !bytes.Equal(backup, oldCipher) {
		t.Fatal("recovery ciphertext was lost", err)
	}
	info, err := os.Stat(previousPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("incorrect recovery backup permissions", info, err)
	}
	current, err := os.ReadFile(options.Config)
	if err != nil || !bytes.Equal(current, oldConfig) {
		t.Fatal("configuration was not restored", err)
	}
	if result != (Result{}) {
		t.Fatal("returned a success result after rollback failure", result)
	}
}

func TestCredentialDropInChangesDuringInputAreNotOverwritten(t *testing.T) {
	for _, change := range []string{"edited", "created", "removed"} {
		t.Run(change, func(t *testing.T) {
			store, options, tools := testCredentialStore(t)
			if change != "created" {
				if _, err := store.set(t.Context(), options, strings.NewReader("old-secret")); err != nil {
					t.Fatal(err)
				}
			}
			cipherPath := filepath.Join(store.directory, "default.cred")
			var oldCipher credentialFile
			if change != "created" {
				var err error
				oldCipher, err = snapshotCredentialFile(cipherPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			oldConfig, err := os.ReadFile(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.dropIns, "credential-default.conf")
			edited := []byte("[Service]\n# Concurrent local configuration\n")
			tools.onEncrypt = func() {
				if change == "removed" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, edited, 0644); err != nil {
					t.Fatal(err)
				}
			}
			result, err := store.set(t.Context(), options, strings.NewReader("new-secret"))
			if err == nil || !strings.Contains(err.Error(), "systemd drop-in changed during credential input") {
				t.Fatal("concurrent drop-in change was not reported", err)
			}
			current, err := os.ReadFile(path)
			if change == "removed" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("removed drop-in was recreated", err)
				}
			} else if err != nil || !bytes.Equal(current, edited) {
				t.Fatal("concurrent drop-in edits were lost", err)
			}
			cipher, err := snapshotCredentialFile(cipherPath)
			if err != nil || cipher.exists != oldCipher.exists || !bytes.Equal(cipher.data, oldCipher.data) {
				t.Fatal("ciphertext was not restored", err)
			}
			if _, err := os.Stat(cipherPath + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed update left a rollback backup", err)
			}
			current, err = os.ReadFile(options.Config)
			if err != nil || !bytes.Equal(current, oldConfig) {
				t.Fatal("configuration was changed", err)
			}
			if result != (Result{}) {
				t.Fatal("returned a success result after concurrent drop-in change", result)
			}
		})
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
	_, err := store.set(t.Context(), options, strings.NewReader("secret"))
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
			path := options.Config
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
			if _, err := store.set(t.Context(), options, strings.NewReader("secret")); err == nil {
				t.Fatal("symlink was accepted")
			}
			data, _ := os.ReadFile(dest)
			if string(data) != "untouched" {
				t.Fatal("symlink target was modified")
			}
		})
	}
}
