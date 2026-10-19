package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func gc(ctx context.Context, config string, dryRun bool, outStream io.Writer) error {
	results, err := ghalet.GC(ctx, config, dryRun)
	return printResponse(outStream, commandResponse{Results: results}, err)
}
