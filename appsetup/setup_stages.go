package appsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

func prepareSetup(options Options) (preparedConfig, error) {
	if err := options.Validate(); err != nil {
		return preparedConfig{}, err
	}
	scope, err := normalizeScope(options.Scope)
	if err != nil {
		return preparedConfig{}, err
	}
	options.Scope = scope
	if options.Listen == "" {
		options.Listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return preparedConfig{}, errors.New("--listen must use a loopback IP address and port")
	}
	prepared, err := setupConfig(options)
	if err != nil {
		return preparedConfig{}, err
	}
	return prepared, nil
}

func lockSetup(directory, id string) (*os.File, error) {
	lock, err := fshelper.LockFile(filepath.Join(directory, id+".setup.lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("another setup for this credential is in progress: %w", err)
	}
	return lock, err
}

type setupFiles struct {
	metadata, key, diskKey string
}

func setupPaths(directory string, options Options) setupFiles {
	files := setupFiles{
		metadata: filepath.Join(directory, options.credentialName+".app.json"),
		key:      filepath.Join(directory, options.credentialName+".pem"),
	}
	files.diskKey = files.key
	if options.Systemd {
		files.diskKey = filepath.Join(directory, options.credentialName+".cred")
		files.key = "/run/credentials/ghalet.service/" + options.credentialName
	}
	return files
}

func (s service) registerApp(ctx context.Context, options Options, files setupFiles, owner, permission string, output io.Writer) (savedApp, Result, error) {
	for _, path := range []string{files.diskKey, files.diskKey + ".previous"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return savedApp{}, Result{}, errors.New("credential already exists without setup metadata; import the existing App or inspect the retained key")
		}
	}
	var account account
	if err := s.api.request(ctx, http.MethodGet, "/users/"+url.PathEscape(owner), "", &account); err != nil {
		return savedApp{}, Result{}, fmt.Errorf("identify App owner: %w", err)
	}
	if !strings.EqualFold(account.Login, owner) || (account.Type != "User" && account.Type != "Organization") ||
		(!strings.Contains(options.Scope, "/") && account.Type != "Organization") {
		return savedApp{}, Result{}, errors.New("target owner is not an eligible user or organization")
	}
	if _, err := fmt.Fprintf(output, "Target: %s (scale set %s, runner group %s)\nApp owner: %s\nRequested permission: %s (write)\n", options.Scope, options.ScaleSet, settings.RunnerGroupName(options.RunnerGroup), owner, permission); err != nil {
		return savedApp{}, Result{}, err
	}
	var state savedApp
	err := s.create(ctx, options, options.Scope, account.Type, permission, output, func(created app) error {
		var err error
		state, err = s.persistApp(ctx, options, files, created, owner)
		return err
	})
	partial := Result{ClientID: state.ClientID, KeyPath: state.KeyPath, MetadataPath: files.metadata, Target: options.credentialName}
	return state, partial, err
}

func (s service) persistApp(ctx context.Context, options Options, files setupFiles, created app, owner string) (savedApp, error) {
	if created.ID <= 0 || created.ClientID == "" || !identifier.MatchString(created.Slug) || created.PEM == "" {
		return savedApp{}, errors.New("GitHub returned incomplete App credentials; inspect the created App before retrying")
	}
	// The consumed Manifest code cannot recover the key; don't abandon it
	// on cancellation or while another credential operation holds the lock.
	storeContext := context.WithoutCancel(ctx)
	storeOptions := credentialstore.Options{
		Name: options.credentialName, Type: "github-app", Stdin: true, NoDefault: true, NoReplace: true,
	}
	var stored credentialstore.Result
	var err error
	for {
		stored, err = s.saveKey(storeContext, storeOptions, strings.NewReader(created.PEM))
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	created.PEM = ""
	if err != nil {
		return savedApp{}, fmt.Errorf("save created App key: %w; inspect App %s in GitHub before retrying", err, created.Slug)
	}
	state := savedApp{ID: created.ID, ClientID: created.ClientID, Slug: created.Slug, Owner: created.Owner.Login, Scope: options.Scope, KeyPath: stored.RuntimePath}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return state, err
	}
	if err := fshelper.WriteFileAtomic(files.metadata, data, 0600); err != nil {
		return state, fmt.Errorf("save setup metadata: %w; key retained at %s for App %s", err, files.diskKey, state.Slug)
	}
	if !strings.EqualFold(state.Owner, owner) {
		return state, errors.New("created App owner does not match this target; App/key retained, inspect the App before retrying")
	}
	return state, nil
}

func (state *savedApp) reconcileKeyPath(options Options, keyPath string) {
	if options.Systemd || state.KeyPath == keyPath {
		return
	}
	// Reconcile only the storage base, never the managed credential directory or key.
	recordedBase := filepath.Dir(filepath.Dir(state.KeyPath))
	if state.KeyPath == filepath.Join(recordedBase, "credentials", options.credentialName+".pem") {
		base, err := fshelper.ResolveDirectory(recordedBase)
		if err == nil && filepath.Join(base, "credentials", options.credentialName+".pem") == keyPath {
			state.KeyPath = keyPath
		}
	}
}

func (s service) installApp(ctx context.Context, options Options, files setupFiles, state savedApp, owner, permission string, newApp bool, output io.Writer) (installation, [sha256.Size]byte, error) {
	if !strings.EqualFold(state.Scope, options.Scope) || !strings.EqualFold(state.Owner, owner) || state.KeyPath != files.key {
		return installation{}, [sha256.Size]byte{}, fmt.Errorf("saved App does not match this target/owner or credential path; inspect %s and App https://github.com/apps/%s; no configuration was changed", files.metadata, state.Slug)
	}
	storage := "Key (unencrypted, owner-only)"
	if options.Systemd {
		storage = "Encrypted key"
	}
	if _, err := fmt.Fprintf(output, "Saved App: https://github.com/apps/%s\n%s: %s\nSetup metadata: %s\n", state.Slug, storage, files.diskKey, files.metadata); err != nil {
		return installation{}, [sha256.Size]byte{}, err
	}
	if err := ctx.Err(); err != nil {
		return installation{}, [sha256.Size]byte{}, fmt.Errorf("setup interrupted: %w; App/key retained, rerun setup to resume", err)
	}
	if err := s.browse(ctx, options.NoBrowser || newApp, installationURL(state.Slug), output); err != nil {
		return installation{}, [sha256.Size]byte{}, err
	}
	if _, err := fmt.Fprintln(output, "Waiting for installation on the target; rerun the same command to resume if interrupted."); err != nil {
		return installation{}, [sha256.Size]byte{}, err
	}
	installed, verifiedKey, err := s.waitInstallation(ctx, state, permission)
	if err != nil {
		return installation{}, [sha256.Size]byte{}, fmt.Errorf("installation not configured: %w; App/key retained, rerun the same command to resume", err)
	}
	return installed, verifiedKey, nil
}

func (prepared preparedConfig) confirm(ctx context.Context, state savedApp, installationID int64, input *os.File, output io.Writer) ([]byte, error) {
	options := prepared.options
	credential := settings.Credential{
		Type: "github-app", ClientID: state.ClientID, PrivateKeyFile: state.KeyPath, InstallationID: installationID,
	}
	destination := "defaults.credential"
	if options.Target {
		destination = "target credential"
		prepared.managed.SetTarget(settings.ManagedTarget{
			Scope: prepared.target.URL, RunnerGroup: prepared.target.RunnerGroup, ScaleSetName: prepared.target.ScaleSetName,
			Credential: credential,
		})
	} else {
		prepared.managed.Defaults.Credential = &credential
		for i, target := range prepared.managed.Targets {
			key, _ := settings.TargetKey(target.Scope, target.RunnerGroup, target.ScaleSetName)
			if key == prepared.target.ID {
				prepared.managed.Targets = append(prepared.managed.Targets[:i], prepared.managed.Targets[i+1:]...)
				break
			}
		}
	}
	if _, err := settings.ParseConfigWithManaged(options.Config, prepared.proposed, prepared.managed); err != nil {
		return nil, fmt.Errorf("validate proposed configuration: %w", err)
	}
	if _, err := fmt.Fprintf(output, "Save %s in managed file %s:\n  type: github-app\n  clientId: %s\n  privateKeyFile: %s\n  installationId: %d\nExplicit target credentials take precedence.\n", destination, prepared.managedPath, state.ClientID, state.KeyPath, installationID); err != nil {
		return nil, err
	}
	if !prepared.targetExists {
		if _, err := fmt.Fprintf(output, "Add target to %s:\n  scope: %s\n  scaleSet: %s\n  runnerGroup: %s\n", options.Config, options.Scope, options.ScaleSet, settings.RunnerGroupName(options.RunnerGroup)); err != nil {
			return nil, err
		}
	}
	if options.Overwrite {
		if _, err := fmt.Fprintln(output, "Replace defaults.credential; targets inheriting defaults will use this installation."); err != nil {
			return nil, err
		}
	}
	if !options.Yes {
		if _, err := fmt.Fprint(output, "Write these configuration changes? [y/N] "); err != nil {
			return nil, err
		}
		answer, err := readConfirmation(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("cannot read setup confirmation: %w; App/key retained, rerun setup to resume", err)
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return nil, errors.New("configuration not changed; App/key retained, rerun setup to resume")
		}
	}
	return prepared.managed.Marshal()
}

func (s service) commitSetup(ctx context.Context, prepared preparedConfig, directory, keyPath string, verifiedKey [sha256.Size]byte, after []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	credentialLock, err := credentialstore.LockDirectory(directory)
	if err != nil {
		return err
	}
	defer credentialLock.Close()
	key, err := s.readKey(ctx, keyPath)
	if err != nil {
		return err
	}
	matches := sha256.Sum256(key) == verifiedKey
	clear(key)
	if !matches {
		return errors.New("saved App key changed during setup; configuration not changed; restore the matching App key before retrying")
	}
	if s.provision != nil {
		if err := s.provision(ctx, prepared.options.credentialName); err != nil {
			return fmt.Errorf("provision systemd App key: %w; App/key and setup metadata retained, rerun setup to resume", err)
		}
	}
	configPath, err := fshelper.ResolveParent(prepared.options.Config)
	if err != nil {
		return err
	}
	if err := fshelper.EnsureDirectory(filepath.Dir(configPath), 0700); err != nil {
		return err
	}
	configLock, err := fshelper.LockFile(configPath + ".lock")
	if err != nil {
		return err
	}
	defer configLock.Close()
	current, err := os.ReadFile(prepared.options.Config)
	if err != nil && !(prepared.before == nil && errors.Is(err, os.ErrNotExist)) {
		return err
	}
	if (prepared.before == nil && err == nil) || !bytes.Equal(current, prepared.before) {
		return errors.New("configuration changed during setup; saved App is retained, rerun setup")
	}
	staticPath, mode := prepared.options.Config, os.FileMode(0600)
	if prepared.before != nil {
		staticPath, err = filepath.EvalSymlinks(staticPath)
		if err != nil {
			return err
		}
		info, err := os.Stat(staticPath)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	}
	if err := settings.WriteManaged(prepared.managedPath, prepared.managedBefore, after); err != nil {
		return err
	}
	if bytes.Equal(prepared.before, prepared.proposed) {
		return nil
	}
	if err := fshelper.WriteFileAtomic(staticPath, prepared.proposed, mode); err != nil {
		// Keep both snapshots consistent if the static target could not be saved.
		if prepared.managedBefore != nil {
			return errors.Join(err, settings.WriteManaged(prepared.managedPath, after, prepared.managedBefore))
		}
		empty, marshalErr := (&settings.Managed{}).Marshal()
		if marshalErr != nil {
			return errors.Join(err, marshalErr)
		}
		return errors.Join(err, settings.WriteManaged(prepared.managedPath, after, empty))
	}
	return nil
}
