package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func doctor(ctx context.Context, opts options, outStream, errStream io.Writer) error {
	opts.control.Command = "doctor"
	return ghalet.Control(ctx, opts.config, opts.control, outStream, errStream)
}
