package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Songmu/ghalet"
	settings "github.com/Songmu/ghalet/config"
)

func run(ctx context.Context, path string) error {
	config, err := settings.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ghalet.RunDaemon(ctx, config)
}
