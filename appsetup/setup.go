// Package appsetup creates a private GitHub App from a Manifest and configures
// one GitHub.com target without starting or reconfiguring the daemon.
package appsetup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	settings "github.com/Songmu/ghalet/config"
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
	prepared, err := prepareSetup(options)
	if err != nil {
		return Result{}, err
	}
	options = prepared.options
	parts := strings.Split(options.Scope, "/")
	owner := parts[0]
	permission := "administration"
	if len(parts) == 1 {
		permission = "organization_self_hosted_runners"
	}
	directory, err := s.prepare(ctx)
	if err != nil {
		return Result{}, err
	}
	files := setupPaths(directory, options)
	reserved := []string{
		files.key, files.diskKey, files.diskKey + ".previous", files.metadata,
		filepath.Join(directory, options.ID+".setup.lock"), filepath.Join(directory, ".lock"),
	}
	if options.Systemd {
		reserved = append(reserved, filepath.Join(credentialstore.SystemdDropInDirectory, "credential-"+options.ID+".conf"))
	}
	if err := settings.CheckManagedPaths(prepared.managedPath, options.Config, reserved...); err != nil {
		return Result{}, err
	}
	lock, err := lockSetup(directory, options.ID)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	state, err := loadApp(files.metadata)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	newApp := errors.Is(err, os.ErrNotExist)
	if newApp {
		var partial Result
		state, partial, err = s.registerApp(ctx, options, files, owner, permission, output)
		if err != nil {
			return partial, err
		}
	}
	state.reconcileKeyPath(options, files.key)
	result := Result{ClientID: state.ClientID, KeyPath: state.KeyPath, MetadataPath: files.metadata, Target: options.ID}
	installed, verifiedKey, err := s.installApp(ctx, options, files, state, owner, permission, newApp, output)
	if err != nil {
		return result, err
	}
	result.InstallationID = installed.ID
	after, err := prepared.confirm(ctx, state, installed.ID, input, output)
	if err != nil {
		return result, err
	}
	return result, s.commitSetup(ctx, prepared, directory, state.KeyPath, verifiedKey, after)
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
