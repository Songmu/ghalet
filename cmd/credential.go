package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Songmu/ghalet/credentialstore"
)

func systemd(ctx context.Context, args []string, config string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] != "credential" {
		fmt.Fprintln(errOut, "Usage: ghalet systemd credential set NAME [--prompt|--file PATH|--stdin] [options]")
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return errors.New("systemd requires the credential subcommand")
	}
	return credential(ctx, args[1:], config, out, errOut)
}

func credential(ctx context.Context, args []string, config string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprintln(errOut, "Usage: ghalet systemd credential set NAME [--prompt|--file PATH|--stdin] [options]")
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return errors.New("systemd credential requires the set subcommand")
	}
	options, err := parseCredentialOptions(args[1:], config, errOut)
	if err != nil {
		return err
	}
	result, err := credentialstore.Set(ctx, options, os.Stdin)
	if err != nil {
		return err
	}
	return printCredentialResult(out, options, result)
}

func parseCredentialOptions(args []string, config string, errOut io.Writer) (credentialstore.Options, error) {
	options := credentialstore.Options{}
	if config == "" {
		config = "/etc/ghalet/config.yaml"
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.Name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ghalet systemd credential set", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&options.Type, "type", "pat", "pat or github-app")
	fs.BoolVar(&options.Prompt, "prompt", false, "ask for a PAT without displaying it")
	fs.StringVar(&options.File, "file", "", "owner-only file containing a PAT or App private key")
	fs.BoolVar(&options.Stdin, "stdin", false, "read the secret from standard input")
	fs.StringVar(&options.Config, "config", config, "configuration to update with defaults.credential")
	fs.StringVar(&options.ClientID, "client-id", "", "GitHub App client ID (not a secret)")
	fs.BoolVar(&options.NoDefault, "no-default", false, "register without changing defaults.credential")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: ghalet systemd credential set NAME [--prompt|--file PATH|--stdin] [options]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if options.Name == "" && fs.NArg() == 1 {
		options.Name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return options, errors.New("systemd credential set accepts exactly one name; secrets must not be passed as arguments")
	}
	if err := options.Validate(); err != nil {
		return options, err
	}
	absolute, err := filepath.Abs(options.Config)
	if err != nil {
		return options, err
	}
	options.Config = absolute
	return options, nil
}

func printCredentialResult(out io.Writer, options credentialstore.Options, result credentialstore.Result) error {
	var err error
	field := "tokenFile"
	if options.Type == "github-app" {
		field = "privateKeyFile"
	}
	if _, err := fmt.Fprintf(out, "Registered credential %s.\nEncrypted file: %s\nSystemd drop-in: %s\n%s: %s\n",
		options.Name, result.EncryptedPath, result.DropInPath, field, result.RuntimePath); err != nil {
		return err
	}
	if !options.NoDefault {
		if _, err := fmt.Fprintf(out, "Updated defaults.credential in %s.\n", options.Config); err != nil {
			return err
		}
	}
	if result.PreviousPath != "" {
		_, err = fmt.Fprintf(out, "Previous encrypted credential: %s\nDrain successfully, restart ghalet, and verify all affected targets before revoking the old credential and deleting this backup.\n", result.PreviousPath)
	} else {
		_, err = fmt.Fprintln(out, "The service was not started or restarted. Validate the configuration before starting it.")
	}
	return err
}
