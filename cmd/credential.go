package cmd

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
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
	"github.com/Songmu/ghalet/fshelper"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

const maxCredentialSize = 1024 * 1024

var credentialName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type credentialOptions struct {
	name, kind, file, config, clientID string
	prompt, stdin, noDefault           bool
}

func credential(ctx context.Context, args []string, config string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprintln(errOut, "Usage: ghalet credential set NAME [--prompt|--file PATH|--stdin] [options]")
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return errors.New("credential requires the set subcommand")
	}
	options, err := parseCredentialOptions(args[1:], config, errOut)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("credential set requires Linux and a systemd system service")
	}
	if os.Geteuid() != 0 {
		return errors.New("credential set requires root; run with sudo (no secret was requested)")
	}
	if options.prompt {
		terminal := exec.CommandContext(ctx, "/bin/sh", "-c", "test -t 0")
		terminal.Stdin = os.Stdin
		if terminal.Run() != nil {
			return errors.New("--prompt requires an interactive terminal; use --file or --stdin for automation")
		}
	}
	for _, tool := range []string{"systemd-creds", "systemctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("credential set requires %s: %w", tool, err)
		}
	}
	if options.prompt {
		if _, err := exec.LookPath("systemd-ask-password"); err != nil {
			return fmt.Errorf("--prompt requires systemd-ask-password: %w", err)
		}
	}
	store := credentialStore{
		directory: "/etc/ghalet/credentials",
		dropIns:   "/etc/systemd/system/ghalet.service.d",
		run:       runCredentialCommand,
	}
	return store.set(ctx, options, os.Stdin, out)
}

func parseCredentialOptions(args []string, config string, errOut io.Writer) (credentialOptions, error) {
	options := credentialOptions{}
	if config == "" {
		config = "/etc/ghalet/config.yaml"
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ghalet credential set", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&options.kind, "type", "pat", "pat or github-app")
	fs.BoolVar(&options.prompt, "prompt", false, "ask for a PAT without displaying it")
	fs.StringVar(&options.file, "file", "", "owner-only file containing a PAT or App private key")
	fs.BoolVar(&options.stdin, "stdin", false, "read the secret from standard input")
	fs.StringVar(&options.config, "config", config, "configuration to update with defaults.credential")
	fs.StringVar(&options.clientID, "client-id", "", "GitHub App client ID (not a secret)")
	fs.BoolVar(&options.noDefault, "no-default", false, "register without changing defaults.credential")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: ghalet credential set NAME [--prompt|--file PATH|--stdin] [options]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if options.name == "" && fs.NArg() == 1 {
		options.name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return options, errors.New("credential set accepts exactly one name; secrets must not be passed as arguments")
	}
	if !credentialName.MatchString(options.name) {
		return options, errors.New("credential name must be 1-64 ASCII letters, digits, underscores or hyphens, starting with a letter or digit")
	}
	if options.kind != "pat" && options.kind != "github-app" {
		return options, errors.New("--type must be pat or github-app")
	}
	inputs := 0
	for _, selected := range []bool{options.prompt, options.file != "", options.stdin} {
		if selected {
			inputs++
		}
	}
	if inputs != 1 {
		return options, errors.New("select exactly one of --prompt, --file or --stdin; input is never requested implicitly")
	}
	if options.kind == "github-app" && options.prompt {
		return options, errors.New("GitHub App keys require --file or --stdin, not --prompt")
	}
	if options.kind == "pat" && options.clientID != "" {
		return options, errors.New("--client-id is only valid with --type github-app")
	}
	absolute, err := filepath.Abs(options.config)
	if err != nil {
		return options, err
	}
	options.config = absolute
	return options, nil
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
	run                credentialCommand
}

func (s credentialStore) set(ctx context.Context, options credentialOptions, input io.Reader, out io.Writer) error {
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
	for path, mode := range map[string]os.FileMode{s.directory: 0700, s.dropIns: 0755} {
		if err := fshelper.EnsureDirectory(path, mode); err != nil {
			return err
		}
		if err := checkCredentialPath(path, true); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(filepath.Join(s.directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another credential operation is in progress: %w", err)
	}
	cipherPath := filepath.Join(s.directory, options.name+".cred")
	previousPath := cipherPath + ".previous"
	dropInPath := filepath.Join(s.dropIns, "credential-"+options.name+".conf")
	cipher, err := snapshotCredentialFile(cipherPath)
	if err != nil {
		return err
	}
	dropIn, err := snapshotCredentialFile(dropInPath)
	if err != nil {
		return err
	}
	dropInData := fmt.Appendf(nil, "[Service]\nLoadCredentialEncrypted=%s:%s\n", options.name, cipherPath)
	if dropIn.exists && !bytes.Equal(dropIn.data, dropInData) {
		return fmt.Errorf("refusing to overwrite customized systemd drop-in %s", dropInPath)
	}
	previous, err := snapshotCredentialFile(previousPath)
	if err != nil {
		return err
	}
	if previous.exists {
		return fmt.Errorf("previous credential backup already exists at %s; finish or roll back the previous rotation before updating again", previousPath)
	}
	var config credentialFile
	var configData []byte
	if !options.noDefault {
		config, err = snapshotCredentialFile(options.config)
		if err != nil {
			return err
		}
		if !config.exists {
			return fmt.Errorf("configuration %s does not exist; copy the package example first, or use --no-default", options.config)
		}
		configData, err = defaultCredentialConfig(config.data, options)
		if err != nil {
			return err
		}
	}
	secret, err := s.readSecret(ctx, options, input)
	if err != nil {
		return err
	}
	defer clear(secret)
	var encrypted bytes.Buffer
	encryptCtx, encryptCancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer encryptCancel()
	if err := s.run(encryptCtx, "systemd-creds", []string{"encrypt", "--name=" + options.name, "--with-key=host", "-", "-"}, bytes.NewReader(secret), &encrypted); err != nil {
		return fmt.Errorf("encrypt credential (existing files unchanged): %w", err)
	}
	if encrypted.Len() == 0 {
		return errors.New("systemd-creds returned an empty encrypted credential; existing files unchanged")
	}
	changes := []credentialFile{}
	write := func(original credentialFile, data []byte, mode os.FileMode) error {
		if err := fshelper.WriteFileAtomic(original.path, data, mode); err != nil {
			return err
		}
		changes = append(changes, original)
		return nil
	}
	rollback := func(cause error) error {
		restoreFailed := false
		for i := len(changes) - 1; i >= 0; i-- {
			original := changes[i]
			if original.path == previousPath && restoreFailed {
				cause = errors.Join(cause, fmt.Errorf("previous encrypted credential retained at %s", previousPath))
				continue
			}
			var err error
			if original.exists {
				err = fshelper.WriteFileAtomic(original.path, original.data, original.mode)
			} else {
				err = os.Remove(original.path)
			}
			if err != nil {
				restoreFailed = true
				cause = errors.Join(cause, fmt.Errorf("restore %s: %w", original.path, err))
			}
		}
		// Reload the restored unit even if the caller canceled the operation.
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeoutForCredential)
		defer cancel()
		if err := s.run(reloadCtx, "systemctl", []string{"daemon-reload"}, nil, io.Discard); err != nil {
			cause = errors.Join(cause, fmt.Errorf("reload restored systemd configuration: %w", err))
		}
		return cause
	}
	if cipher.exists {
		if err := write(previous, cipher.data, 0600); err != nil {
			return err
		}
	}
	if err := write(cipher, encrypted.Bytes(), 0600); err != nil {
		return rollback(err)
	}
	currentDropIn, err := snapshotCredentialFile(dropInPath)
	if err != nil {
		return rollback(fmt.Errorf("recheck systemd drop-in %s: %w", dropInPath, err))
	}
	if currentDropIn.exists != dropIn.exists || !bytes.Equal(currentDropIn.data, dropIn.data) {
		return rollback(errors.New("systemd drop-in changed during credential input; refusing to overwrite it"))
	}
	if err := write(dropIn, dropInData, 0644); err != nil {
		return rollback(err)
	}
	if !options.noDefault {
		current, err := os.ReadFile(options.config)
		if err != nil || !bytes.Equal(current, config.data) {
			return rollback(errors.New("configuration changed during credential input; refusing to overwrite it"))
		}
		if err := write(config, configData, 0600); err != nil {
			return rollback(err)
		}
	}
	reloadCtx, cancel := context.WithTimeout(ctx, commandTimeoutForCredential)
	defer cancel()
	if err := s.run(reloadCtx, "systemctl", []string{"daemon-reload"}, nil, io.Discard); err != nil {
		return rollback(fmt.Errorf("reload systemd configuration: %w", err))
	}
	field := "tokenFile"
	if options.kind == "github-app" {
		field = "privateKeyFile"
	}
	if _, err := fmt.Fprintf(out, "Registered credential %s.\nEncrypted file: %s\nSystemd drop-in: %s\n%s: /run/credentials/ghalet.service/%s\n",
		options.name, cipherPath, dropInPath, field, options.name); err != nil {
		return err
	}
	if !options.noDefault {
		if _, err := fmt.Fprintf(out, "Updated defaults.credential in %s.\n", options.config); err != nil {
			return err
		}
	}
	if cipher.exists {
		_, err = fmt.Fprintf(out, "Previous encrypted credential: %s\nDrain successfully, restart ghalet, and verify all affected targets before revoking the old credential and deleting this backup.\n", previousPath)
	} else {
		_, err = fmt.Fprintln(out, "The service was not started or restarted. Validate the configuration before starting it.")
	}
	return err
}

const commandTimeoutForCredential = 30 * time.Second

func (s credentialStore) readSecret(ctx context.Context, options credentialOptions, input io.Reader) ([]byte, error) {
	if options.prompt {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		var data bytes.Buffer
		if err := s.run(ctx, "systemd-ask-password", []string{"--timeout=2min", "PAT for ghalet credential " + options.name + ":"}, nil, &data); err != nil {
			clear(data.Bytes())
			return nil, fmt.Errorf("PAT input canceled or failed: %w", err)
		}
		input = bytes.NewReader(data.Bytes())
		defer clear(data.Bytes())
	}
	if options.file != "" {
		info, err := os.Lstat(options.file)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("--file requires a regular file, not a symlink or pipe")
		}
		file, err := os.OpenFile(options.file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("input file must be accessible only to its owner (normally chmod 600)")
		}
		input = file
	}
	data, err := io.ReadAll(io.LimitReader(input, maxCredentialSize+1))
	if err != nil {
		clear(data)
		return nil, errors.New("read credential input failed")
	}
	secret := bytes.TrimSpace(data)
	if len(data) > maxCredentialSize || len(secret) == 0 {
		clear(data)
		return nil, errors.New("credential must be nonempty and no larger than 1 MiB")
	}
	if options.kind == "pat" {
		if bytes.IndexFunc(secret, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
			clear(data)
			return nil, errors.New("PAT must be a single nonempty ASCII token without whitespace")
		}
	} else {
		block, rest := pem.Decode(secret)
		if block == nil || len(bytes.TrimSpace(rest)) != 0 {
			clear(data)
			return nil, errors.New("GitHub App input must contain one unencrypted RSA PEM private key")
		}
		var key any
		switch block.Type {
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		default:
			err = errors.New("unsupported PEM block")
		}
		_, rsaKey := key.(*rsa.PrivateKey)
		if err != nil || !rsaKey {
			clear(data)
			return nil, errors.New("GitHub App input must contain an unencrypted RSA PEM private key")
		}
	}
	return secret, nil
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

func defaultCredentialConfig(data []byte, options credentialOptions) ([]byte, error) {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return nil, errors.New("cannot parse the selected configuration")
	}
	if len(file.Docs) != 1 {
		return nil, errors.New("configuration must contain exactly one YAML document")
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, errors.New("configuration must be a YAML mapping")
	}
	var config ghalet.Config
	if err := yaml.NodeToValue(root, &config, yaml.Strict()); err != nil {
		return nil, errors.New("configuration contains invalid or unknown fields; validate it before registering a credential")
	}
	cred := map[string]string{"type": options.kind}
	path := "/run/credentials/ghalet.service/" + options.name
	if options.kind == "pat" {
		cred["tokenFile"] = path
	} else {
		clientID := options.clientID
		if clientID == "" && config.Defaults.Credential != nil && config.Defaults.Credential.Type == "github-app" {
			clientID = config.Defaults.Credential.ClientID
		}
		if clientID == "" {
			return nil, errors.New("GitHub App defaults require --client-id or an existing defaults.credential.clientId")
		}
		cred["clientId"], cred["privateKeyFile"] = clientID, path
	}
	comments := yaml.CommentMap{}
	var document yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &document, yaml.UseOrderedMap(), yaml.CommentToMap(comments)); err != nil {
		return nil, errors.New("cannot read configuration mappings")
	}
	defaults := yaml.MapSlice{{Key: "credential", Value: cred}}
	replaced := false
	for i := range document {
		if document[i].Key == "defaults" {
			document[i].Value, replaced = defaults, true
			break
		}
	}
	if !replaced {
		document = append(document, yaml.MapItem{Key: "defaults", Value: defaults})
	}
	return yaml.MarshalWithOptions(document, yaml.WithComment(comments))
}
