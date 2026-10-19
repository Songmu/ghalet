package cmd

import (
	"context"
	"io"

	"github.com/Songmu/ghalet"
)

func destroy(ctx context.Context, opts options, outStream, errStream io.Writer) error {
	opts.control.Command = "destroy"
	return ghalet.Control(ctx, opts.config, opts.control, outStream, errStream)
}
