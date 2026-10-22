package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Songmu/ghalet/appsetup"
)

func app(ctx context.Context, args []string, config string, out, errOut io.Writer, systemd bool) error {
	command := "app"
	if systemd {
		command = "systemd app"
	}
	if len(args) == 0 || args[0] != "setup" {
		fmt.Fprintf(errOut, "Usage: ghalet %s setup --scope OWNER/REPO|ORG --scale-set NAME [options]\n", command)
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			return flag.ErrHelp
		}
		return errors.New("app requires the setup subcommand")
	}
	options, err := parseAppOptions(args[1:], config, errOut, systemd)
	if err != nil {
		return err
	}
	result, err := appsetup.Setup(ctx, options, os.Stdin, out)
	if err != nil {
		return err
	}
	message := "The daemon was not started or reconfigured. Validate the configuration, then start or apply it explicitly."
	if systemd {
		message = "The service was not started or restarted. Validate the configuration, then start it explicitly; an existing service needs a normal stop and successful shutdown before starting it with updated systemd credentials."
	}
	_, err = fmt.Fprintf(out, "Saved managed authentication for %s (scale set %s) with installation %d.\n%s\n", options.Scope, options.ScaleSet, result.InstallationID, message)
	return err
}

func parseAppOptions(args []string, config string, errOut io.Writer, systemd bool) (appsetup.Options, error) {
	command := "app"
	if systemd {
		command = "systemd app"
		if config == "" {
			config = "/etc/ghalet/config.yaml"
		}
	}
	options := appsetup.Options{Config: config, Systemd: systemd}
	fs := flag.NewFlagSet("ghalet "+command+" setup", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&options.Config, "config", config, "static configuration whose managed file to update")
	fs.StringVar(&options.Scope, "scope", "", "required GitHub.com scope: OWNER/REPO or ORG")
	fs.StringVar(&options.ScaleSet, "scale-set", "", "required scale set name and runs-on label")
	fs.StringVar(&options.RunnerGroup, "runner-group", "", "organization runner group (default group when omitted)")
	fs.StringVar(&options.Listen, "listen", "127.0.0.1:0", "loopback callback address; use a fixed port for SSH forwarding")
	fs.BoolVar(&options.NoBrowser, "no-browser", systemd, "print browser URLs without opening them (default for systemd)")
	fs.BoolVar(&options.Yes, "yes", false, "write the displayed managed information without a confirmation prompt")
	fs.BoolVar(&options.Target, "target", false, "save the credential for this target instead of defaults")
	fs.BoolVar(&options.Overwrite, "overwrite", false, "replace an existing defaults.credential")
	fs.Usage = func() {
		fmt.Fprintf(errOut, "Usage: ghalet %s setup --scope OWNER/REPO|ORG --scale-set NAME [options]\n", command)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if fs.NArg() != 0 {
		return options, errors.New("app setup does not accept positional arguments; use --scope and --scale-set")
	}
	return options, options.Validate()
}
