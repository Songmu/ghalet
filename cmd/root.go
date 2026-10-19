package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Songmu/ghalet"
)

const cmdName = "ghalet"

type options struct {
	config  string
	control ghalet.ControlOptions
}

// Run executes the ghalet command.
func Run(ctx context.Context, argv []string, outStream, errStream io.Writer) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(errStream)
	versionFlag := fs.Bool("version", false, "display version")
	var opts options
	fs.StringVar(&opts.config, "config", "", "configuration path (default ~/.config/ghalet/config.yaml)")
	// Accept common flags both before and after the command.
	command := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		command, argv = argv[0], argv[1:]
	}
	if command == "config" {
		if len(argv) == 0 || argv[0] != "validate" {
			return errors.New("usage: ghalet config validate [--config path]")
		}
		command, argv = "validate", argv[1:]
	}
	fs.StringVar(&opts.control.Target, "target", "", "target id (required for destroy; default all targets for drain)")
	fs.BoolVar(&opts.control.DryRun, "dry-run", false, "show garbage collection without modifying files")
	fs.StringVar(&opts.control.RunnerGroup, "runner-group", "", "existing organization runner group")
	fs.StringVar(&opts.control.ScaleSet, "scale-set", "", "explicit scale set name to destroy")
	fs.Usage = func() {
		fmt.Fprintln(errStream, "Usage: ghalet <run|config validate|status|doctor|drain|gc|destroy> [options]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *versionFlag {
		return printVersion(outStream)
	}
	if command == "" && fs.NArg() > 0 {
		command = fs.Arg(0)
		remaining := fs.Args()[1:]
		if command == "config" && len(remaining) > 0 && remaining[0] == "validate" {
			command, remaining = "validate", remaining[1:]
		}
		if err := fs.Parse(remaining); err != nil {
			return err
		}
	}
	if *versionFlag {
		return printVersion(outStream)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	run, ok := commands[command]
	if !ok {
		fs.Usage()
		return errors.New("a supported command is required")
	}
	return run(ctx, opts, outStream, errStream)
}

func printVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "%s v%s (rev:%s)\n", cmdName, ghalet.Version, ghalet.Revision)
	return err
}
