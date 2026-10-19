package ghalet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const cmdName = "ghalet"

// Run executes the ghalet command.
func Run(ctx context.Context, argv []string, outStream, errStream io.Writer) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(errStream)
	versionFlag := fs.Bool("version", false, "display version")
	configFlag := fs.String("config", "", "configuration path (default ~/.config/ghalet/config.yaml)")
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
	target := fs.String("target", "", "target id (default all targets)")
	dryRun := fs.Bool("dry-run", false, "show garbage collection without modifying files")
	scope := fs.String("scope", "", "explicit scope to destroy")
	group := fs.String("runner-group", "", "existing organization runner group")
	set := fs.String("scale-set", "", "explicit scale set name to destroy")
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
	path, err := configPath(*configFlag)
	if err != nil {
		return err
	}
	switch command {
	case "run", "validate", "status", "doctor", "drain", "gc", "destroy":
	default:
		fs.Usage()
		return errors.New("a supported command is required")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("selected config: %w", err)
	}
	if command == "validate" || command == "run" {
		config, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if command == "validate" {
			_, err := fmt.Fprintln(outStream, "Configuration is valid.")
			return err
		}
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx, config, outStream)
	}
	request := controlRequest{Command: command, Target: *target, DryRun: *dryRun, Scope: *scope, RunnerGroup: *group, ScaleSet: *set}
	if command == "destroy" {
		request.Command = "destroy-plan"
		terminal := exec.Command("/bin/sh", "-c", "test -t 0")
		terminal.Stdin = os.Stdin
		if terminal.Run() != nil {
			return errors.New("destroy requires an interactive terminal and explicit yes confirmation; nothing was deleted")
		}
	}
	response, connectionErr := controlCall(ctx, path, request)
	if connectionErr == nil {
		if command == "destroy" && response.Error == "" {
			if err := confirmDestroy(response.Plan, os.Stdin, errStream); err != nil {
				return err
			}
			response, err = controlCall(ctx, path, controlRequest{Command: "destroy", Confirmation: response.Plan.Confirmation})
			if err != nil {
				return err
			}
		}
		return printResponse(outStream, response)
	}
	if command != "gc" && command != "destroy" {
		return fmt.Errorf("connect to running daemon: %w", connectionErr)
	}
	config, err := LoadConfig(path)
	if err != nil {
		return err
	}
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return fmt.Errorf("control connection failed (%v); refusing direct operation: %w", connectionErr, err)
	}
	defer lock.close()
	controlLock, err := lockControl(path)
	if err != nil {
		return fmt.Errorf("refusing direct operation while the config's control address is owned: %w", err)
	}
	defer controlLock.Close()
	// Recheck after acquiring the workRoot lock. A socket failure alone is
	// never evidence that a daemon is absent.
	if _, err := controlCall(ctx, path, controlRequest{Command: "status"}); err == nil {
		return errors.New("daemon became available; retry the operation through its control socket")
	}
	d, err := newDaemon(config, errStream)
	if err != nil {
		return err
	}
	defer func() {
		d.cancel()
		d.workers.Wait()
	}()
	if err := d.initialize(ctx, true); err != nil {
		return err
	}
	if command == "gc" {
		return printResponse(outStream, d.handle(ctx, request))
	}
	response = d.handle(ctx, request)
	if response.Error != "" {
		return printResponse(outStream, response)
	}
	if err := confirmDestroy(response.Plan, os.Stdin, errStream); err != nil {
		return err
	}
	engineCtx, stopEngine := context.WithCancel(ctx)
	engineResult := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-engineCtx.Done():
				engineResult <- nil
				return
			case <-ticker.C:
				d.mu.Lock()
				d.reconcile()
				d.mu.Unlock()
			case event := <-d.events:
				d.mu.Lock()
				d.processEvent(event)
				d.mu.Unlock()
			}
		}
	}()
	response = d.handle(ctx, controlRequest{Command: "destroy", Confirmation: response.Plan.Confirmation})
	stopEngine()
	engineErr := <-engineResult
	return errors.Join(printResponse(outStream, response), engineErr)
}

func confirmDestroy(plan *destroyPlan, input io.Reader, out io.Writer) error {
	if plan == nil {
		return errors.New("missing deletion plan")
	}
	if _, err := fmt.Fprintf(out, "Delete scale set %q (id %d) at %s, group %q? Type yes to drain and delete: ", plan.ScaleSet, plan.ScaleSetID, plan.URL, plan.RunnerGroup); err != nil {
		return err
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("deletion not approved; nothing was deleted")
	}
	return nil
}

func printResponse(out io.Writer, response controlResponse) error {
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func runDaemon(ctx context.Context, config *Config, out io.Writer) error {
	lock, err := lockWorkRoot(config.Runner.WorkRoot)
	if err != nil {
		return err
	}
	defer lock.close()
	server, err := listenControl(config.Path)
	if err != nil {
		return err
	}
	defer server.close()
	d, err := newDaemon(config, out)
	if err != nil {
		return err
	}
	defer d.cancel()
	if err := d.initialize(ctx, false); err != nil {
		return err
	}
	go server.serve(d)
	d.log.Info("Daemon ready", "event", "daemon.ready", "config", config.Path, "capacity", config.Capacity.MaxRunners)
	return d.serve(ctx)
}

func printVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "%s v%s (rev:%s)\n", cmdName, version, revision)
	return err
}
