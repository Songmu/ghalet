package cmd

import (
	"context"
	"io"
	"log/slog"

	"github.com/Songmu/ghalet"
)

func gc(ctx context.Context, config string, dryRun bool, outStream io.Writer, logger *slog.Logger) error {
	results, err := ghalet.GC(ctx, config, dryRun, logger)
	return printResponse(outStream, commandResponse{Results: results}, err)
}
