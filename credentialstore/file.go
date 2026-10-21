package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/fshelper"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Directory returns the ordinary credential directory for the current user/service.
func Directory() (string, error) {
	root, err := ghalet.StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "credentials"), nil
}

// PrepareDirectory creates a private credential directory without making new
// state ancestors inaccessible to a separate runner execution user.
func PrepareDirectory() (string, error) {
	directory, err := Directory()
	if err != nil {
		return "", err
	}
	return directory, ensureFileDirectory(directory)
}

func ensureFileDirectory(directory string) error {
	if err := fshelper.EnsureDirectory(filepath.Dir(directory), 0711); err != nil {
		return err
	}
	if err := checkCredentialPath(filepath.Dir(directory), true); err != nil {
		return err
	}
	if err := fshelper.EnsureDirectory(directory, 0700); err != nil {
		return err
	}
	return CheckPrivatePath(directory, true)
}

// SetFile registers an owner-only, unencrypted credential without changing a daemon.
func SetFile(ctx context.Context, options Options, input io.Reader) (Result, error) {
	if err := options.Validate(); err != nil {
		return Result{}, err
	}
	directory, err := Directory()
	if err != nil {
		return Result{}, err
	}
	return setFile(ctx, directory, options, input)
}

// CheckPrivatePath verifies an owner-only file or directory owned by this user.
func CheckPrivatePath(path string, directory bool) error {
	if err := checkCredentialPath(path, directory); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("credential path must be accessible only to its owner: %s", path)
	}
	return nil
}

// LockDirectory prevents ordinary credential replacement until the returned
// file is closed. Acquire it before a configuration lock when using both.
func LockDirectory(directory string) (*os.File, error) {
	if err := CheckPrivatePath(directory, true); err != nil {
		return nil, err
	}
	return fshelper.LockFile(filepath.Join(directory, ".lock"))
}

func setFile(ctx context.Context, directory string, options Options, input io.Reader) (Result, error) {
	return setFileWithWriter(ctx, directory, options, input, fshelper.WriteFileAtomic)
}

func setFileWithWriter(ctx context.Context, directory string, options Options, input io.Reader, writeAtomic func(string, []byte, os.FileMode) error) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := ensureFileDirectory(directory); err != nil {
		return Result{}, err
	}
	lock, err := LockDirectory(directory)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	extension := ".pat"
	if options.Type == "github-app" {
		extension = ".pem"
	}
	path := filepath.Join(directory, options.Name+extension)
	previousPath := path + ".previous"
	for _, candidate := range []string{path, previousPath} {
		if err := CheckPrivatePath(candidate, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	}
	original, err := snapshotCredentialFile(path)
	if err != nil {
		return Result{}, err
	}
	defer clear(original.data)
	if options.NoReplace && original.exists {
		return Result{}, fmt.Errorf("credential already exists at %s; existing credential unchanged", path)
	}
	previous, err := snapshotCredentialFile(previousPath)
	if err != nil {
		return Result{}, err
	}
	defer clear(previous.data)
	if previous.exists {
		return Result{}, fmt.Errorf("previous credential backup already exists at %s; finish or roll back the previous rotation", previousPath)
	}
	var config credentialFile
	var configData []byte
	if !options.NoDefault {
		config, err = snapshotConfigFile(options.Config)
		if err != nil {
			return Result{}, err
		}
		if !config.exists {
			return Result{}, errors.New("configuration does not exist; create it first or use --no-default")
		}
		configData, err = credentialConfig(config.data, options, path)
		if err != nil {
			return Result{}, err
		}
	}
	if options.Prompt {
		file, ok := input.(*os.File)
		if !ok || !term.IsTerminal(int(file.Fd())) {
			return Result{}, errors.New("--prompt requires an interactive terminal; use --file or --stdin")
		}
		data, err := readPassword(ctx, options.PromptOutput)
		if err != nil {
			clear(data)
			return Result{}, fmt.Errorf("PAT input canceled or failed: %w", err)
		}
		defer clear(data)
		input = bytes.NewReader(data)
		options.Prompt, options.Stdin = false, true
	}
	secret, err := (credentialStore{}).readSecret(ctx, options, input)
	if err != nil {
		return Result{}, err
	}
	defer clear(secret)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	changes := []credentialFile{}
	write := func(file credentialFile, data []byte) error {
		if err := writeAtomic(file.path, data, 0600); err != nil {
			return err
		}
		changes = append(changes, file)
		return nil
	}
	rollback := func(cause error) error {
		restoreFailed := false
		for i := len(changes) - 1; i >= 0; i-- {
			file := changes[i]
			if file.path == previousPath && restoreFailed {
				cause = errors.Join(cause, fmt.Errorf("previous credential retained at %s", previousPath))
				continue
			}
			var err error
			if file.exists {
				err = writeAtomic(file.path, file.data, file.mode)
			} else {
				err = os.Remove(file.path)
			}
			if err != nil {
				restoreFailed = true
				cause = errors.Join(cause, fmt.Errorf("restore %s: %w", file.path, err))
			}
		}
		return cause
	}
	if !options.NoDefault {
		configLock, err := fshelper.LockFile(config.path + ".lock")
		if err != nil {
			return Result{}, err
		}
		defer configLock.Close()
		current, err := os.ReadFile(config.path)
		if err != nil || !bytes.Equal(current, config.data) {
			return Result{}, errors.New("configuration changed during credential input; existing credentials unchanged")
		}
	}
	if original.exists {
		if err := write(previous, original.data); err != nil {
			return Result{}, err
		}
	}
	if err := write(original, secret); err != nil {
		return Result{}, rollback(err)
	}
	if !options.NoDefault {
		if err := write(config, configData); err != nil {
			return Result{}, rollback(err)
		}
	}
	result := Result{RuntimePath: path}
	if original.exists {
		result.PreviousPath = previousPath
	}
	return result, nil
}

func readPassword(ctx context.Context, output io.Writer) (secret []byte, resultErr error) {
	if output == nil {
		return nil, errors.New("prompt output is required")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := term.Restore(int(tty.Fd()), state); err != nil {
			clear(secret)
			secret = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("restore terminal: %w", err))
		}
	}()
	if _, err := fmt.Fprint(output, "PAT: "); err != nil {
		return nil, err
	}
	defer func() {
		if _, err := fmt.Fprint(output, "\r\n"); err != nil {
			clear(secret)
			secret = nil
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var password []byte
	var buffer [256]byte
	defer clear(buffer[:])
	fail := func(err error) ([]byte, error) {
		clear(password)
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		ready, err := unix.Poll([]unix.PollFd{{Fd: int32(tty.Fd()), Events: unix.POLLIN}}, 100)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		if ready == 0 {
			continue
		}
		n, err := unix.Read(int(tty.Fd()), buffer[:])
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		if n == 0 {
			return fail(io.EOF)
		}
		for _, value := range buffer[:n] {
			switch value {
			case '\r', '\n':
				return password, nil
			case 3:
				return fail(context.Canceled)
			case 4:
				return fail(io.EOF)
			case 8, 127:
				if len(password) > 0 {
					password[len(password)-1] = 0
					password = password[:len(password)-1]
				}
			case 21:
				clear(password)
				password = password[:0]
			default:
				if len(password) == maxCredentialSize {
					return fail(errors.New("credential exceeds 1 MiB"))
				}
				password = append(password, value)
			}
		}
	}
}
