package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Songmu/ghalet"
)

func run(ctx context.Context, path string, logger *slog.Logger) error {
	config, err := ghalet.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ghalet.RunDaemon(ctx, config, logger)
}
