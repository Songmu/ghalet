package cmd

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Songmu/ghalet"
)

func run(ctx context.Context, opts options, outStream, errStream io.Writer) error {
	config, err := ghalet.LoadConfig(opts.config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ghalet.RunDaemon(ctx, config, outStream)
}
