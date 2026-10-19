package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func status(ctx context.Context, config string, outStream io.Writer) error {
	result, err := ghalet.GetStatus(ctx, config)
	return printResponse(outStream, commandResponse{Status: result}, err)
}
