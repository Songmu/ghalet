package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Songmu/ghalet"
)

func run(ctx context.Context, path string) error {
	config, err := ghalet.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ghalet.RunDaemon(ctx, config)
}
