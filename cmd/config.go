package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/Songmu/ghalet"
)

func validate(ctx context.Context, opts options, outStream, errStream io.Writer) error {
	if _, err := ghalet.LoadConfig(opts.config); err != nil {
		return err
	}
	_, err := fmt.Fprintln(outStream, "Configuration is valid.")
	return err
}
