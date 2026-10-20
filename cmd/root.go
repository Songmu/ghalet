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
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(errStream)
	versionFlag := fs.Bool("version", false, "display version")
	config := fs.String("config", "", "configuration path (default ~/.config/ghalet/config.yaml)")
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
	if command == "systemd" {
		return systemd(ctx, argv, "", outStream, errStream)
	}
	dryRun := fs.Bool("dry-run", false, "show garbage collection without modifying files")
	fs.Usage = func() {
		fmt.Fprintln(errStream, "Usage: ghalet <run|apply|config validate|systemd credential set|status|doctor|gc> [options]")
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
		if command == "systemd" {
			return systemd(ctx, remaining, *config, outStream, errStream)
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
	logStream := errStream
	if command == "run" {
		logStream = outStream
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logStream, &slog.HandlerOptions{
		ReplaceAttr: logreduct.Default.ReplaceAttr,
	})))
	commands := map[string]func() error{
		"run":      func() error { return run(ctx, *config) },
		"apply":    func() error { return apply(ctx, *config, outStream) },
		"validate": func() error { return validate(*config, outStream) },
		"status":   func() error { return status(ctx, *config, outStream) },
		"doctor":   func() error { return doctor(ctx, *config, outStream) },
		"gc":       func() error { return gc(ctx, *config, *dryRun, outStream) },
	}
	run, ok := commands[command]
	if !ok {
		fs.Usage()
		return errors.New("a supported command is required")
	}
	return run()
}

func printVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "%s v%s (rev:%s)\n", cmdName, ghalet.Version, ghalet.Revision)
	return err
}
