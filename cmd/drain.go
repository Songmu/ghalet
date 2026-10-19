package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func drain(ctx context.Context, config, target string, outStream io.Writer) error {
	result, err := ghalet.Drain(ctx, config, target)
	return printResponse(outStream, commandResponse{Status: result}, err)
}
