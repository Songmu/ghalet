// Package appsetup creates a private GitHub App from a Manifest and configures
// one GitHub.com target without starting or reconfiguring the daemon.
package appsetup

import (
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

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

// Result contains only public App identifiers and local paths.
type Result struct {
	ClientID, KeyPath, MetadataPath, Target string
	InstallationID                          int64
}

type savedApp struct {
	ID       int64  `json:"appId"`
	ClientID string `json:"clientId"`
	Slug     string `json:"slug"`
	Owner    string `json:"owner"`
	Scope    string `json:"scope"`
	KeyPath  string `json:"keyPath"`
}

type service struct {
	api       apiClient
	open      func(context.Context, string) error
	saveKey   func(context.Context, credentialstore.Options, io.Reader) (credentialstore.Result, error)
	prepare   func(context.Context) (string, error)
	readKey   func(context.Context, string) ([]byte, error)
	provision func(context.Context, string) error
	interval  time.Duration
	timeout   time.Duration
}

// Setup creates or resumes an App registration, then confirms and updates a target.
// input supplies confirmation and remains open when setup returns.
func Setup(ctx context.Context, options Options, input *os.File, output io.Writer) (Result, error) {
	s := service{
		api: apiClient{base: "https://api.github.com", http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("GitHub API redirects are not allowed")
			},
		}},
		open: openBrowser, saveKey: credentialstore.SetFile,
		prepare:  func(context.Context) (string, error) { return credentialstore.PrepareDirectory() },
		readKey:  func(_ context.Context, path string) ([]byte, error) { return readPrivate(path) },
		interval: 2 * time.Second, timeout: 15 * time.Minute,
	}
	if options.Systemd {
		s.prepare, s.saveKey = credentialstore.PrepareSystemd, credentialstore.SaveSystemdAppKey
		s.provision = credentialstore.ProvisionSystemdAppKey
		s.readKey = func(ctx context.Context, path string) ([]byte, error) {
			return credentialstore.ReadSystemdAppKey(ctx, filepath.Base(path))
		}
	}
	return s.run(ctx, options, input, output)
}

func (s service) run(ctx context.Context, options Options, input *os.File, output io.Writer) (Result, error) {
	if err := options.Validate(); err != nil {
		return Result{}, err
	}
	scope, err := normalizeScope(options.Scope)
	if err != nil {
		return Result{}, err
	}
	options.Scope = scope
	if options.Listen == "" {
		options.Listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return Result{}, errors.New("--listen must use a loopback IP address and port")
	}
	options, before, proposed, target, err := setupConfig(options)
	if err != nil {
		return Result{}, err
	}
	options.ID = target.ID
	parts := strings.Split(scope, "/")
	owner := parts[0]
	permission := "administration"
	if len(parts) == 1 {
		permission = "organization_self_hosted_runners"
	}
	directory, err := s.prepare(ctx)
	if err != nil {
		return Result{}, err
	}
	lockPath := filepath.Join(directory, options.ID+".setup.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err := credentialstore.CheckPrivatePath(lockPath, false); err != nil {
		return Result{}, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Result{}, fmt.Errorf("another setup for this credential is in progress: %w", err)
	}
	metadataPath := filepath.Join(directory, options.ID+".app.json")
	keyPath := filepath.Join(directory, options.ID+".pem")
	diskKeyPath := keyPath
	if options.Systemd {
		diskKeyPath = filepath.Join(directory, options.ID+".cred")
		keyPath = "/run/credentials/ghalet.service/" + options.ID
	}
	state, err := loadApp(metadataPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	newApp := errors.Is(err, os.ErrNotExist)
	if newApp {
		for _, path := range []string{diskKeyPath, diskKeyPath + ".previous"} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return Result{}, errors.New("credential already exists without setup metadata; use a different --id or import the existing App")
			}
		}
		var account account
		if err := s.api.request(ctx, http.MethodGet, "/users/"+url.PathEscape(owner), "", &account); err != nil {
			return Result{}, fmt.Errorf("identify App owner: %w", err)
		}
		if !strings.EqualFold(account.Login, owner) || (account.Type != "User" && account.Type != "Organization") ||
			(len(parts) == 1 && account.Type != "Organization") {
			return Result{}, errors.New("target owner is not an eligible user or organization")
		}
		if _, err := fmt.Fprintf(output, "Target: %s (%s)\nApp owner: %s\nRequested permission: %s (write)\n", target.ID, scope, owner, permission); err != nil {
			return Result{}, err
		}
		err := s.create(ctx, options, scope, account.Type, permission, output, func(created app) error {
			if created.ID <= 0 || created.ClientID == "" || !identifier.MatchString(created.Slug) || created.PEM == "" {
				return errors.New("GitHub returned incomplete App credentials; inspect the created App before retrying")
			}
			// The consumed Manifest code cannot recover the key; don't abandon it
			// on cancellation or while another credential operation holds the lock.
			storeContext := context.WithoutCancel(ctx)
			storeOptions := credentialstore.Options{
				Name: options.ID, Type: "github-app", Stdin: true, NoDefault: true, NoReplace: true,
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
				return fmt.Errorf("save created App key: %w; inspect App %s in GitHub before retrying", err, created.Slug)
			}
			state = savedApp{ID: created.ID, ClientID: created.ClientID, Slug: created.Slug, Owner: created.Owner.Login, Scope: scope, KeyPath: stored.RuntimePath}
			data, err := json.MarshalIndent(state, "", "  ")
			if err != nil {
				return err
			}
			if err := fshelper.WriteFileAtomic(metadataPath, data, 0600); err != nil {
				return fmt.Errorf("save setup metadata: %w; key retained at %s for App %s", err, diskKeyPath, state.Slug)
			}
			if !strings.EqualFold(state.Owner, owner) {
				return errors.New("created App owner does not match this target; App/key retained, inspect the App before retrying")
			}
			return nil
		})
		if err != nil {
			return Result{ClientID: state.ClientID, KeyPath: state.KeyPath, MetadataPath: metadataPath, Target: target.ID}, err
		}
	}
	result := Result{ClientID: state.ClientID, KeyPath: state.KeyPath, MetadataPath: metadataPath, Target: target.ID}
	if !strings.EqualFold(state.Scope, scope) || !strings.EqualFold(state.Owner, owner) ||
		state.KeyPath != keyPath {
		return result, fmt.Errorf("saved App does not match this target/owner or credential path; inspect %s and App https://github.com/apps/%s; no configuration was changed", metadataPath, state.Slug)
	}
	storage := "Key (unencrypted, owner-only)"
	if options.Systemd {
		storage = "Encrypted key"
	}
	if _, err := fmt.Fprintf(output, "Saved App: https://github.com/apps/%s\n%s: %s\nSetup metadata: %s\n", state.Slug, storage, diskKeyPath, metadataPath); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("setup interrupted: %w; App/key retained, rerun setup to resume", err)
	}
	if err := s.browse(ctx, options.NoBrowser || newApp, installationURL(state.Slug), output); err != nil {
		return result, err
	}
	if _, err := fmt.Fprintln(output, "Waiting for installation on the target; rerun the same command to resume if interrupted."); err != nil {
		return result, err
	}
	installed, verifiedKey, err := s.waitInstallation(ctx, state, permission)
	if err != nil {
		return result, fmt.Errorf("installation not configured: %w; App/key retained, rerun the same command to resume", err)
	}
	result.InstallationID = installed.ID
	after, err := targetConfig(proposed, target.ID, state, installed.ID)
	if err != nil {
		return result, err
	}
	if _, err := ghalet.ParseConfig(options.Config, after); err != nil {
		return result, fmt.Errorf("validate proposed configuration: %w", err)
	}
	if _, err := fmt.Fprintf(output, "Configure target %s in %s:\n  scope: %s\n  credential.type: github-app\n  credential.clientId: %s\n  credential.privateKeyFile: %s\n  installationId: %d\n", target.ID, options.Config, scope, state.ClientID, state.KeyPath, installed.ID); err != nil {
		return result, err
	}
	if !options.Yes {
		if _, err := fmt.Fprint(output, "Write these configuration changes? [y/N] "); err != nil {
			return result, err
		}
		answer, err := readConfirmation(ctx, input)
		if err != nil {
			return result, fmt.Errorf("cannot read setup confirmation: %w; App/key retained, rerun setup to resume", err)
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return result, errors.New("configuration not changed; App/key retained, rerun setup to resume")
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	credentialLock, err := credentialstore.LockDirectory(directory)
	if err != nil {
		return result, err
	}
	defer credentialLock.Close()
	key, err := s.readKey(ctx, state.KeyPath)
	if err != nil {
		return result, err
	}
	matches := sha256.Sum256(key) == verifiedKey
	clear(key)
	if !matches {
		return result, errors.New("saved App key changed during setup; configuration not changed; restore the matching App key before retrying")
	}
	if s.provision != nil {
		if err := s.provision(ctx, options.ID); err != nil {
			return result, fmt.Errorf("provision systemd App key: %w; App/key and setup metadata retained, rerun setup to resume", err)
		}
	}
	if err := writeConfig(options.Config, before, after); err != nil {
		return result, err
	}
	return result, nil
}

func loadApp(path string) (savedApp, error) {
	var state savedApp
	data, err := readPrivate(path)
	if err != nil {
		return state, err
	}
	defer clear(data)
	if err := json.Unmarshal(data, &state); err != nil || state.ID <= 0 || state.ClientID == "" ||
		!identifier.MatchString(state.Slug) || state.Owner == "" || state.Scope == "" || state.KeyPath == "" {
		return savedApp{}, errors.New("invalid saved App metadata; inspect it before retrying")
	}
	return state, nil
}

func readPrivate(path string) ([]byte, error) {
	if err := fshelper.CheckDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := credentialstore.CheckPrivatePath(path, false); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		clear(data)
		return nil, errors.New("cannot read saved App file (limit 1 MiB)")
	}
	return data, nil
}

func (s service) waitInstallation(ctx context.Context, state savedApp, permission string) (installation, [sha256.Size]byte, error) {
	key, err := s.readKey(ctx, state.KeyPath)
	if err != nil {
		return installation{}, [sha256.Size]byte{}, err
	}
	defer clear(key)
	for {
		token, err := appToken(state.ClientID, key)
		if err != nil {
			return installation{}, [sha256.Size]byte{}, err
		}
		var installed installation
		err = s.api.request(ctx, http.MethodGet, installationPath(state.Scope), token, &installed)
		if err == nil {
			if installed.ID <= 0 || installed.AppID != state.ID || !strings.EqualFold(installed.Account.Login, state.Owner) ||
				installed.Permissions[permission] != "write" || installed.SuspendedAt != nil {
				return installation{}, [sha256.Size]byte{}, errors.New("installation is suspended or does not match the App, owner, or required write permission")
			}
			return installed, sha256.Sum256(key), nil
		}
		var status httpError
		if !errors.As(err, &status) || status != httpError(http.StatusNotFound) {
			return installation{}, [sha256.Size]byte{}, err
		}
		select {
		case <-ctx.Done():
			return installation{}, [sha256.Size]byte{}, ctx.Err()
		case <-time.After(s.interval):
		}
	}
}
