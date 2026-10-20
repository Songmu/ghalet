// Package appsetup creates a private GitHub App from a Manifest and configures
// one GitHub.com target without starting or reconfiguring the daemon.
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

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

// Options selects an App credential and a single configured GitHub.com target.
type Options struct {
	Name, Config, Target, Listen string
	NoBrowser, Yes               bool
}

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
	api      apiClient
	open     func(context.Context, string) error
	interval time.Duration
	timeout  time.Duration
}

// Setup creates or resumes an App registration, then confirms and updates a target.
// input supplies confirmation and remains open when setup returns.
func Setup(ctx context.Context, options Options, input *os.File, output io.Writer) (Result, error) {
	return (service{
		api: apiClient{base: "https://api.github.com", http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("GitHub API redirects are not allowed")
			},
		}},
		open: openBrowser, interval: 2 * time.Second, timeout: 15 * time.Minute,
	}).run(ctx, options, input, output)
}

func (s service) run(ctx context.Context, options Options, input *os.File, output io.Writer) (Result, error) {
	if err := (credentialstore.Options{Name: options.Name, Type: "github-app", Stdin: true, NoDefault: true}).Validate(); err != nil {
		return Result{}, err
	}
	if options.Listen == "" {
		options.Listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return Result{}, errors.New("--listen must use a loopback IP address and port")
	}
	config, err := ghalet.LoadConfig(options.Config)
	if err != nil {
		return Result{}, err
	}
	before, err := configSnapshot(config.Path)
	if err != nil {
		return Result{}, err
	}
	config, err = ghalet.LoadConfig(config.Path)
	if err != nil {
		return Result{}, err
	}
	current, err := configSnapshot(config.Path)
	if err != nil || !bytes.Equal(before, current) {
		return Result{}, errors.New("configuration changed while loading it; retry setup")
	}
	var target ghalet.Target
	for _, candidate := range config.Targets {
		if options.Target == candidate.ID || (options.Target == "" && len(config.Targets) == 1) {
			target = candidate
		}
	}
	if target.ID == "" {
		return Result{}, errors.New("select one configured target with --target ID")
	}
	address, err := url.Parse(target.URL)
	if err != nil || address.Scheme != "https" || address.Host != "github.com" {
		return Result{}, errors.New("setup currently supports GitHub.com targets only")
	}
	scope := strings.Trim(address.Path, "/")
	parts := strings.Split(scope, "/")
	owner := parts[0]
	permission := "administration"
	if len(parts) == 1 {
		permission = "organization_self_hosted_runners"
	}
	directory, err := credentialstore.PrepareDirectory()
	if err != nil {
		return Result{}, err
	}
	lockPath := filepath.Join(directory, options.Name+".setup.lock")
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
	metadataPath := filepath.Join(directory, options.Name+".app.json")
	state, err := loadApp(metadataPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if errors.Is(err, os.ErrNotExist) {
		keyPath := filepath.Join(directory, options.Name+".pem")
		for _, path := range []string{keyPath, keyPath + ".previous"} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return Result{}, errors.New("credential already exists without setup metadata; use a different name or import the existing App")
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
		created, err := s.create(ctx, options, scope, account.Type, permission, output)
		if err != nil {
			return Result{}, err
		}
		if created.ID <= 0 || created.ClientID == "" || !identifier.MatchString(created.Slug) || created.PEM == "" {
			return Result{}, errors.New("GitHub returned incomplete App credentials; inspect the created App before retrying")
		}
		stored, err := credentialstore.SetFile(ctx, credentialstore.Options{
			Name: options.Name, Type: "github-app", Stdin: true, NoDefault: true, NoReplace: true,
		}, strings.NewReader(created.PEM))
		created.PEM = ""
		if err != nil {
			return Result{}, fmt.Errorf("save created App key: %w; inspect App %s in GitHub before retrying", err, created.Slug)
		}
		state = savedApp{ID: created.ID, ClientID: created.ClientID, Slug: created.Slug, Owner: created.Owner.Login, Scope: scope, KeyPath: stored.RuntimePath}
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return Result{}, err
		}
		if err := fshelper.WriteFileAtomic(metadataPath, data, 0600); err != nil {
			return Result{}, fmt.Errorf("save setup metadata: %w; key retained at %s for App %s", err, stored.RuntimePath, state.Slug)
		}
	}
	result := Result{ClientID: state.ClientID, KeyPath: state.KeyPath, MetadataPath: metadataPath, Target: target.ID}
	if !strings.EqualFold(state.Scope, scope) || !strings.EqualFold(state.Owner, owner) ||
		state.KeyPath != filepath.Join(directory, options.Name+".pem") {
		return result, fmt.Errorf("saved App does not match this target/owner or credential path; inspect %s and App https://github.com/apps/%s; no configuration was changed", metadataPath, state.Slug)
	}
	if _, err := fmt.Fprintf(output, "Saved App: https://github.com/apps/%s\nKey (unencrypted, owner-only): %s\nSetup metadata: %s\n", state.Slug, state.KeyPath, metadataPath); err != nil {
		return result, err
	}
	if err := s.browse(ctx, options.NoBrowser, "https://github.com/apps/"+state.Slug+"/installations/new", output); err != nil {
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
	after, err := targetConfig(before, target.ID, state, installed.ID)
	if err != nil {
		return result, err
	}
	if _, err := fmt.Fprintf(output, "Update target %s only:\n  credential.type: github-app\n  credential.clientId: %s\n  credential.privateKeyFile: %s\n  installationId: %d\n", target.ID, state.ClientID, state.KeyPath, installed.ID); err != nil {
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
	key, err := readPrivate(state.KeyPath)
	if err != nil {
		return result, err
	}
	matches := sha256.Sum256(key) == verifiedKey
	clear(key)
	if !matches {
		return result, errors.New("saved App key changed during setup; configuration not changed; restore the matching App key before retrying")
	}
	if err := writeConfig(config.Path, before, after); err != nil {
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
	key, err := readPrivate(state.KeyPath)
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
