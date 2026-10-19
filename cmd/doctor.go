package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func doctor(ctx context.Context, config string, outStream io.Writer) error {
	result, err := ghalet.Doctor(ctx, config)
	return printResponse(outStream, commandResponse{Status: result}, err)
}
