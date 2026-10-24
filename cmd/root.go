package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/ghalet/logreduct"
)

const cmdName = "ghalet"

// Run executes the ghalet command.
func Run(ctx context.Context, argv []string, outStream, errStream io.Writer) error {
	if len(argv) > 0 {
		switch argv[0] {
		case "__ghalet_runner_user":
			return runnerUser(argv[1:])
		case "__ghalet_runner_guard":
			return runnerGuard(argv[1:])
		}
	}
	command, err := parseCommand(argv, errStream)
	if err != nil {
		return err
	}
	if command.version {
		return printVersion(outStream)
	}
	switch command.name {
	case "systemd":
		return systemd(ctx, command.args, command.config, outStream, errStream)
	case "credential":
		return runCredential(ctx, command.args, command.config, outStream, errStream, false)
	case "app":
		return app(ctx, command.args, command.config, outStream, errStream, false)
	}
	logStream := errStream
	if command.name == "run" {
		logStream = outStream
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logStream, &slog.HandlerOptions{
		ReplaceAttr: logreduct.Default.ReplaceAttr,
	})))
	switch command.name {
	case "run":
		return run(ctx, command.config)
	case "apply":
		return apply(ctx, command.config, outStream)
	case "validate":
		return validate(command.config, outStream)
	case "list":
		return list(ctx, command.config, outStream)
	case "status":
		return status(ctx, command.config, command.failOnError, outStream)
	case "gc":
		return gc(ctx, command.config, command.dryRun, outStream)
	default:
		command.usage()
		return errors.New("a supported command is required")
	}
}

type commandLine struct {
	name, config string
	args         []string
	version      bool
	dryRun       bool
	failOnError  bool
	usage        func()
}

func parseCommand(argv []string, errStream io.Writer) (commandLine, error) {
	var command commandLine
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(errStream)
	fs.BoolVar(&command.version, "version", false, "display version")
	fs.StringVar(&command.config, "config", "", "configuration path (management commands discover a single daemon; otherwise ~/.config/ghalet/config.yaml)")
	// Accept common flags both before and after the command.
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		command.name, argv = argv[0], argv[1:]
	}
	if command.name == "config" {
		if len(argv) == 0 || argv[0] != "validate" {
			return command, errors.New("usage: ghalet config validate [--config path]")
		}
		command.name, argv = "validate", argv[1:]
	}
	switch command.name {
	case "systemd", "credential", "app":
		command.args = argv
		return command, nil
	}
	fs.BoolVar(&command.dryRun, "dry-run", false, "show garbage collection without modifying files")
	fs.BoolVar(&command.failOnError, "fail-on-error", false, "exit nonzero if status reports diagnostic problems")
	fs.Usage = func() {
		fmt.Fprintln(errStream, "Usage: ghalet <run|apply|config validate|app setup|credential set|systemd app setup|systemd credential set|list|status|gc> [options]")
		fs.PrintDefaults()
	}
	command.usage = fs.Usage
	if err := fs.Parse(argv); err != nil {
		return command, err
	}
	if command.version {
		return command, nil
	}
	if command.name == "" && fs.NArg() > 0 {
		command.name = fs.Arg(0)
		remaining := fs.Args()[1:]
		if command.name == "config" && len(remaining) > 0 && remaining[0] == "validate" {
			command.name, remaining = "validate", remaining[1:]
		}
		switch command.name {
		case "systemd", "credential", "app":
			command.args = remaining
			return command, nil
		}
		if err := fs.Parse(remaining); err != nil {
			return command, err
		}
	}
	if !command.version && fs.NArg() != 0 {
		return command, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if command.failOnError && command.name != "status" {
		return command, errors.New("--fail-on-error is only supported by status")
	}
	return command, nil
}

func printVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "%s v%s (rev:%s)\n", cmdName, ghalet.Version, ghalet.Revision)
	return err
}
