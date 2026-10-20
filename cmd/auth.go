package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Songmu/ghalet/appsetup"
)

func auth(ctx context.Context, args []string, config string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] != "setup" {
		fmt.Fprintln(errOut, "Usage: ghalet auth setup NAME [--target ID] [options]")
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return errors.New("auth requires the setup subcommand")
	}
	options, err := parseAuthOptions(args[1:], config, errOut)
	if err != nil {
		return err
	}
	result, err := appsetup.Setup(ctx, options, os.Stdin, out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Configured target %s with installation %d.\nThe daemon was not started or reconfigured. Validate the configuration, then start or apply it explicitly.\n", result.Target, result.InstallationID)
	return err
}

func parseAuthOptions(args []string, config string, errOut io.Writer) (appsetup.Options, error) {
	options := appsetup.Options{Config: config}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.Name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("ghalet auth setup", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&options.Config, "config", config, "configuration containing the target")
	fs.StringVar(&options.Target, "target", "", "one target ID (required when there are multiple targets)")
	fs.StringVar(&options.Listen, "listen", "127.0.0.1:0", "loopback callback address; use a fixed port for SSH forwarding")
	fs.BoolVar(&options.NoBrowser, "no-browser", false, "print browser URLs without opening them")
	fs.BoolVar(&options.Yes, "yes", false, "write the displayed target configuration without a confirmation prompt")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: ghalet auth setup NAME [--target ID] [options]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if options.Name == "" && fs.NArg() == 1 {
		options.Name = fs.Arg(0)
	} else if fs.NArg() != 0 || options.Name == "" {
		return options, errors.New("auth setup requires exactly one credential name")
	}
	return options, nil
}
