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
	return runCredential(ctx, args, config, out, errOut, true)
}

func runCredential(ctx context.Context, args []string, config string, out, errOut io.Writer, encrypted bool) error {
	command := "credential"
	if encrypted {
		command = "systemd credential"
	}
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprintf(errOut, "Usage: ghalet %s set NAME [--prompt|--file PATH|--stdin] [options]\n", command)
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return fmt.Errorf("%s requires the set subcommand", command)
	}
	options, err := parseCredentialOptionsFor(args[1:], config, errOut, encrypted)
	if err != nil {
		return err
	}
	var result credentialstore.Result
	if encrypted {
		result, err = credentialstore.Set(ctx, options, os.Stdin)
	} else {
		if options.Prompt {
			if _, err := fmt.Fprint(errOut, "PAT: "); err != nil {
				return err
			}
			defer fmt.Fprintln(errOut)
		}
		result, err = credentialstore.SetFile(ctx, options, os.Stdin)
	}
	if err != nil {
		return err
	}
	if encrypted {
		return printCredentialResult(out, options, result)
	}
	return printFileCredentialResult(out, options, result)
}

func parseCredentialOptions(args []string, config string, errOut io.Writer) (credentialstore.Options, error) {
	return parseCredentialOptionsFor(args, config, errOut, true)
}

func parseCredentialOptionsFor(args []string, config string, errOut io.Writer, encrypted bool) (credentialstore.Options, error) {
	options := credentialstore.Options{}
	command := "credential"
	if encrypted {
		command = "systemd credential"
	}
	if config == "" {
		if encrypted {
			config = "/etc/ghalet/config.yaml"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return options, err
			}
			config = filepath.Join(home, ".config", "ghalet", "config.yaml")
		}
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.Name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ghalet "+command+" set", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&options.Type, "type", "pat", "pat or github-app")
	fs.BoolVar(&options.Prompt, "prompt", false, "ask for a PAT without displaying it")
	fs.StringVar(&options.File, "file", "", "owner-only file containing a PAT or App private key")
	fs.BoolVar(&options.Stdin, "stdin", false, "read the secret from standard input")
	fs.StringVar(&options.Config, "config", config, "configuration to update with defaults.credential")
	fs.StringVar(&options.ClientID, "client-id", "", "GitHub App client ID (not a secret)")
	fs.BoolVar(&options.NoDefault, "no-default", false, "register without changing defaults.credential")
	fs.Usage = func() {
		fmt.Fprintf(errOut, "Usage: ghalet %s set NAME [--prompt|--file PATH|--stdin] [options]\n", command)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if options.Name == "" && fs.NArg() == 1 {
		options.Name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return options, fmt.Errorf("%s set accepts exactly one name; secrets must not be passed as arguments", command)
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

func printFileCredentialResult(out io.Writer, options credentialstore.Options, result credentialstore.Result) error {
	if _, err := fmt.Fprintf(out, "Registered credential %s.\nCredential file (unencrypted, owner-only): %s\n", options.Name, result.RuntimePath); err != nil {
		return err
	}
	if !options.NoDefault {
		if _, err := fmt.Fprintf(out, "Updated defaults.credential in %s.\n", options.Config); err != nil {
			return err
		}
	}
	if result.PreviousPath != "" {
		if _, err := fmt.Fprintf(out, "Previous credential: %s\nKeep it until all affected targets authenticate with the replacement, then revoke the old credential and remove the backup.\n", result.PreviousPath); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "The daemon was not started, restarted or reconfigured. Validate the configuration before starting or applying it.")
	return err
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
		_, err = fmt.Fprintf(out, "Previous encrypted credential: %s\nStop ghalet normally and verify successful shutdown from its exit result and logs, then start it and verify all affected targets before revoking the old credential and deleting this backup.\n", result.PreviousPath)
	} else {
		_, err = fmt.Fprintln(out, "The service was not started or restarted. Validate the configuration before starting it.")
	}
	return err
}
