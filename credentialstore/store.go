// Package credentialstore manages encrypted systemd credentials for ghalet.
package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Songmu/ghalet"
	configeditor "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/fshelper"
)

var credentialName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Options selects a credential and its explicit secret input source.
type Options struct {
	Name, Type, File, Config, ClientID string
	Prompt, Stdin, NoDefault           bool
	PromptOutput                       io.Writer
	NoReplace                          bool
}

// Result describes the files installed by a successful registration.
type Result struct {
	EncryptedPath, DropInPath, RuntimePath, PreviousPath string
}

// Validate checks the credential identity and input source selection.
func (options Options) Validate() error {
	if !credentialName.MatchString(options.Name) {
		return errors.New("credential name must be 1-64 ASCII letters, digits, underscores or hyphens, starting with a letter or digit")
	}
	if options.Type != "pat" && options.Type != "github-app" {
		return errors.New("--type must be pat or github-app")
	}
	inputs := 0
	for _, selected := range []bool{options.Prompt, options.File != "", options.Stdin} {
		if selected {
			inputs++
		}
	}
	if inputs != 1 {
		return errors.New("select exactly one of --prompt, --file or --stdin; input is never requested implicitly")
	}
	if options.Type == "github-app" && options.Prompt {
		return errors.New("GitHub App keys require --file or --stdin, not --prompt")
	}
	if options.Type == "pat" && options.ClientID != "" {
		return errors.New("--client-id is only valid with --type github-app")
	}
	return nil
}

// Set encrypts and registers a credential for the system ghalet service.
// It reloads systemd configuration but never starts or restarts the service.
// input supplies the secret stream or, for Prompt, the caller's terminal.
func Set(ctx context.Context, options Options, input io.Reader) (Result, error) {
	if err := options.Validate(); err != nil {
		return Result{}, err
	}
	if err := checkSystemd(); err != nil {
		return Result{}, err
	}
	if options.Prompt {
		terminal := exec.CommandContext(ctx, "/bin/sh", "-c", "test -t 0")
		terminal.Stdin = input
		if terminal.Run() != nil {
			return Result{}, errors.New("--prompt requires an interactive terminal; use --file or --stdin for automation")
		}
	}
	if options.Prompt {
		if _, err := exec.LookPath("systemd-ask-password"); err != nil {
			return Result{}, fmt.Errorf("--prompt requires systemd-ask-password: %w", err)
		}
	}
	store, err := systemdStore()
	if err != nil {
		return Result{}, err
	}
	return store.set(ctx, options, input)
}

func checkSystemd() error {
	if runtime.GOOS != "linux" {
		return errors.New("systemd credentials require Linux and a systemd system service")
	}
	if os.Geteuid() != 0 {
		return errors.New("systemd credentials require root; run with sudo (no secret was requested)")
	}
	for _, tool := range []string{"systemd-creds", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("systemd credentials require %s: %w", tool, err)
		}
	}
	return nil
}

func systemdStore() (credentialStore, error) {
	return newSystemdStore("/etc/ghalet/credentials", "/etc/systemd/system/ghalet.service.d")
}

func newSystemdStore(directory, dropIns string) (credentialStore, error) {
	sourceDirectory := directory
	directory, err := fshelper.ResolveParent(directory)
	if err != nil {
		return credentialStore{}, err
	}
	dropIns, err = fshelper.ResolveParent(dropIns)
	if err != nil {
		return credentialStore{}, err
	}
	return credentialStore{
		directory:       directory,
		dropIns:         dropIns,
		sourceDirectory: sourceDirectory,
		run:             runCredentialCommand,
	}, nil
}

// PrepareSystemd checks systemd support and prepares its private credential store.
func PrepareSystemd(ctx context.Context) (string, error) {
	if err := checkSystemd(); err != nil {
		return "", err
	}
	store, err := systemdStore()
	if err != nil {
		return "", err
	}
	if err := store.prepare(ctx); err != nil {
		return "", err
	}
	return store.directory, nil
}

// ReadSystemdAppKey decrypts and validates an App key without printing it.
func ReadSystemdAppKey(ctx context.Context, name string) ([]byte, error) {
	if !credentialName.MatchString(name) {
		return nil, errors.New("invalid systemd credential name")
	}
	if err := checkSystemd(); err != nil {
		return nil, err
	}
	store, err := systemdStore()
	if err != nil {
		return nil, err
	}
	return store.read(ctx, name)
}

// SaveSystemdAppKey exclusively persists a new encrypted App key without
// configuring or reloading the service, so setup can save its resume metadata.
func SaveSystemdAppKey(ctx context.Context, options Options, input io.Reader) (Result, error) {
	if err := options.Validate(); err != nil {
		return Result{}, err
	}
	if options.Type != "github-app" || !options.NoDefault || !options.NoReplace {
		return Result{}, errors.New("saving a setup App key requires github-app, no-default and no-replace")
	}
	if err := checkSystemd(); err != nil {
		return Result{}, err
	}
	store, err := systemdStore()
	if err != nil {
		return Result{}, err
	}
	return store.saveAppKey(ctx, options, input)
}

// ProvisionSystemdAppKey loads an already saved key into the service definition.
// The caller must hold the credential directory lock until configuration commit.
func ProvisionSystemdAppKey(ctx context.Context, name string) error {
	if !credentialName.MatchString(name) {
		return errors.New("invalid systemd credential name")
	}
	if err := checkSystemd(); err != nil {
		return err
	}
	store, err := systemdStore()
	if err != nil {
		return err
	}
	return store.provisionAppKey(ctx, name)
}

func (s credentialStore) read(ctx context.Context, name string) ([]byte, error) {
	if err := fshelper.CheckDirectory(s.directory); err != nil {
		return nil, err
	}
	if err := CheckPrivatePath(s.directory, true); err != nil {
		return nil, err
	}
	path := filepath.Join(s.directory, name+".cred")
	if err := CheckPrivatePath(path, false); err != nil {
		return nil, err
	}
	cipher, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer cancel()
	var secret bytes.Buffer
	defer func() { clear(secret.Bytes()) }()
	if err := s.run(ctx, "systemd-creds", []string{"decrypt", "--name=" + name, "-", "-"}, bytes.NewReader(cipher), &secret); err != nil {
		return nil, fmt.Errorf("decrypt App credential: %w", err)
	}
	return s.readSecret(ctx, Options{Type: "github-app", Stdin: true}, bytes.NewReader(secret.Bytes()))
}

type credentialCommand func(context.Context, string, []string, io.Reader, io.Writer) error

func runCredentialCommand(ctx context.Context, name string, args []string, input io.Reader, output io.Writer) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin, command.Stdout = input, output
	// Tool diagnostics may contain the secret; never relay them to the journal.
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	return command.Run()
}

type credentialStore struct {
	directory, dropIns string
	// Keep unit references stable across layout changes and free of target-path specifiers.
	sourceDirectory string
	run             credentialCommand
}

func (s credentialStore) dropInData(name string) []byte {
	return fmt.Appendf(nil, "[Service]\nLoadCredentialEncrypted=%s:%s\n", name, filepath.Join(s.sourceDirectory, name+".cred"))
}

func (s credentialStore) prepare(ctx context.Context) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer checkCancel()
	var version bytes.Buffer
	if err := s.run(checkCtx, "systemd-creds", []string{"--version"}, nil, &version); err != nil {
		return fmt.Errorf("check systemd-creds: %w", err)
	}
	fields := strings.Fields(version.String())
	if len(fields) < 2 {
		return errors.New("cannot determine systemd-creds version")
	}
	major, err := strconv.Atoi(fields[1])
	if err != nil || major < 250 {
		return errors.New("encrypted credentials require systemd 250 or later")
	}
	if err := fshelper.EnsureDirectory(s.directory, 0700); err != nil {
		return err
	}
	return CheckPrivatePath(s.directory, true)
}

func (s credentialStore) prepareDropIns() error {
	if err := fshelper.EnsureDirectory(s.dropIns, 0755); err != nil {
		return err
	}
	return checkCredentialPath(s.dropIns, true)
}

func (s credentialStore) encrypt(ctx context.Context, name string, secret []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer cancel()
	var encrypted bytes.Buffer
	if err := s.run(ctx, "systemd-creds", []string{"encrypt", "--name=" + name, "--with-key=host", "-", "-"}, bytes.NewReader(secret), &encrypted); err != nil {
		return nil, fmt.Errorf("encrypt credential (existing files unchanged): %w", err)
	}
	if encrypted.Len() == 0 {
		return nil, errors.New("systemd-creds returned an empty encrypted credential; existing files unchanged")
	}
	return encrypted.Bytes(), nil
}

func (s credentialStore) saveAppKey(ctx context.Context, options Options, input io.Reader) (Result, error) {
	if err := s.prepare(ctx); err != nil {
		return Result{}, err
	}
	lock, err := LockDirectory(s.directory)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	path := filepath.Join(s.directory, options.Name+".cred")
	for _, candidate := range []string{path, path + ".previous"} {
		file, err := snapshotCredentialFile(candidate)
		if err != nil {
			return Result{}, err
		}
		if file.exists {
			return Result{}, fmt.Errorf("credential already exists at %s; existing credential unchanged", candidate)
		}
	}
	secret, err := s.readSecret(ctx, options, input)
	if err != nil {
		return Result{}, err
	}
	defer clear(secret)
	cipher, err := s.encrypt(ctx, options.Name, secret)
	if err != nil {
		return Result{}, err
	}
	if err := fshelper.WriteFileAtomic(path, cipher, 0600); err != nil {
		return Result{}, err
	}
	return Result{EncryptedPath: path, RuntimePath: "/run/credentials/ghalet.service/" + options.Name}, nil
}

func (s credentialStore) provisionAppKey(ctx context.Context, name string) error {
	if err := CheckPrivatePath(filepath.Join(s.directory, name+".cred"), false); err != nil {
		return err
	}
	if err := s.prepareDropIns(); err != nil {
		return err
	}
	path := filepath.Join(s.dropIns, "credential-"+name+".conf")
	before, err := snapshotCredentialFile(path)
	if err != nil {
		return err
	}
	data := s.dropInData(name)
	if before.exists && !bytes.Equal(before.data, data) {
		return fmt.Errorf("refusing to overwrite customized systemd drop-in %s", path)
	}
	if !before.exists {
		if err := fshelper.WriteFileAtomic(path, data, 0644); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer cancel()
	if err := s.run(ctx, "systemctl", []string{"daemon-reload"}, nil, io.Discard); err != nil {
		return fmt.Errorf("reload systemd configuration: %w", err)
	}
	return nil
}

func (s credentialStore) set(ctx context.Context, options Options, input io.Reader) (result Result, err error) {
	if err := s.prepare(ctx); err != nil {
		return result, err
	}
	if err := s.prepareDropIns(); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(filepath.Join(s.directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("another credential operation is in progress: %w", err)
	}
	cipherPath := filepath.Join(s.directory, options.Name+".cred")
	previousPath := cipherPath + ".previous"
	dropInPath := filepath.Join(s.dropIns, "credential-"+options.Name+".conf")
	cipher, err := snapshotCredentialFile(cipherPath)
	if err != nil {
		return result, err
	}
	if options.NoReplace && cipher.exists {
		return result, fmt.Errorf("credential already exists at %s; existing credential unchanged", cipherPath)
	}
	dropIn, err := snapshotCredentialFile(dropInPath)
	if err != nil {
		return result, err
	}
	dropInData := s.dropInData(options.Name)
	if dropIn.exists && !bytes.Equal(dropIn.data, dropInData) {
		return result, fmt.Errorf("refusing to overwrite customized systemd drop-in %s", dropInPath)
	}
	previous, err := snapshotCredentialFile(previousPath)
	if err != nil {
		return result, err
	}
	if previous.exists {
		return result, fmt.Errorf("previous credential backup already exists at %s; finish or roll back the previous rotation before updating again", previousPath)
	}
	var config credentialFile
	var configData []byte
	if !options.NoDefault {
		config, err = snapshotConfigFile(options.Config)
		if err != nil {
			return result, err
		}
		if !config.exists {
			return result, fmt.Errorf("configuration %s does not exist; copy the package example first, or use --no-default", options.Config)
		}
		configData, err = defaultCredentialConfig(config.data, options)
		if err != nil {
			return result, err
		}
	}
	secret, err := s.readSecret(ctx, options, input)
	if err != nil {
		return result, err
	}
	defer clear(secret)
	encrypted, err := s.encrypt(ctx, options.Name, secret)
	if err != nil {
		return result, err
	}
	updates := credentialUpdates{writeAtomic: fshelper.WriteFileAtomic}
	rollback := func(cause error) error {
		cause = updates.rollback(cause, previousPath, "previous encrypted credential")
		// Reload the restored unit even if the caller canceled the operation.
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeoutForCredential)
		defer cancel()
		if err := s.run(reloadCtx, "systemctl", []string{"daemon-reload"}, nil, io.Discard); err != nil {
			cause = errors.Join(cause, fmt.Errorf("reload restored systemd configuration: %w", err))
		}
		return cause
	}
	if !options.NoDefault {
		configLock, err := fshelper.LockFile(config.path + ".lock")
		if err != nil {
			return result, err
		}
		defer configLock.Close()
	}
	if cipher.exists {
		if err := updates.write(previous, cipher.data, 0600); err != nil {
			return result, err
		}
	}
	if err := updates.write(cipher, encrypted, 0600); err != nil {
		return result, rollback(err)
	}
	currentDropIn, err := snapshotCredentialFile(dropInPath)
	if err != nil {
		return result, rollback(fmt.Errorf("recheck systemd drop-in %s: %w", dropInPath, err))
	}
	if currentDropIn.exists != dropIn.exists || !bytes.Equal(currentDropIn.data, dropIn.data) {
		return result, rollback(errors.New("systemd drop-in changed during credential input; refusing to overwrite it"))
	}
	if err := updates.write(dropIn, dropInData, 0644); err != nil {
		return result, rollback(err)
	}
	if !options.NoDefault {
		current, err := os.ReadFile(config.path)
		if err != nil || !bytes.Equal(current, config.data) {
			return result, rollback(errors.New("configuration changed during credential input; refusing to overwrite it"))
		}
		if err := updates.write(config, configData, 0600); err != nil {
			return result, rollback(err)
		}
	}
	reloadCtx, cancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer cancel()
	if err := s.run(reloadCtx, "systemctl", []string{"daemon-reload"}, nil, io.Discard); err != nil {
		return result, rollback(fmt.Errorf("reload systemd configuration: %w", err))
	}
	result = Result{
		EncryptedPath: cipherPath,
		DropInPath:    dropInPath,
		RuntimePath:   "/run/credentials/ghalet.service/" + options.Name,
	}
	if cipher.exists {
		result.PreviousPath = previousPath
	}
	return result, nil
}

const commandTimeoutForCredential = 30 * time.Second

func (s credentialStore) readSecret(ctx context.Context, options Options, input io.Reader) ([]byte, error) {
	if options.Prompt {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		var data bytes.Buffer
		if err := s.run(ctx, "systemd-ask-password", []string{"--timeout=2min", "PAT for ghalet systemd credential " + options.Name + ":"}, input, &data); err != nil {
			clear(data.Bytes())
			return nil, fmt.Errorf("PAT input canceled or failed: %w", err)
		}
		input = bytes.NewReader(data.Bytes())
		defer clear(data.Bytes())
	}
	return readCredentialInput(options, input)
}

func checkCredentialPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("credential path must be a regular file or directory, not a symlink: %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("credential path must be owned by the current user and not writable by other users: %s", path)
	}
	return nil
}

type credentialFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotConfigFile(path string) (credentialFile, error) {
	path, err := fshelper.ResolveParent(path)
	if err != nil {
		return credentialFile{}, err
	}
	return snapshotCredentialFile(path)
}

func snapshotCredentialFile(path string) (credentialFile, error) {
	file := credentialFile{path: path}
	if err := fshelper.CheckDirectory(filepath.Dir(path)); err != nil {
		return file, err
	}
	if err := checkCredentialPath(path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return file, nil
		}
		return file, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return file, err
	}
	file.data, err = os.ReadFile(path)
	file.mode, file.exists = info.Mode().Perm(), true
	return file, err
}

func defaultCredentialConfig(data []byte, options Options) ([]byte, error) {
	return credentialConfig(data, options, "/run/credentials/ghalet.service/"+options.Name)
}

func credentialConfig(data []byte, options Options, path string) ([]byte, error) {
	var config ghalet.Config
	editor, err := configeditor.NewEditor(data, &config)
	if err != nil {
		return nil, err
	}
	clientID := options.ClientID
	if options.Type != "pat" {
		if clientID == "" && config.Defaults.Credential != nil && config.Defaults.Credential.Type == "github-app" {
			clientID = config.Defaults.Credential.ClientID
		}
		if clientID == "" {
			return nil, errors.New("GitHub App defaults require --client-id or an existing defaults.credential.clientId")
		}
	}
	editor.SetDefaultCredential(options.Type, clientID, path)
	return editor.Marshal()
}
