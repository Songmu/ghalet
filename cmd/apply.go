package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func apply(ctx context.Context, config string, outStream io.Writer) error {
	result, err := ghalet.Apply(ctx, config)
	return printResponse(outStream, commandResponse{Status: result.Status, Results: result.Results}, err)
}
