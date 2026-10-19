package cmd

import (
	"fmt"
	"io"

	"github.com/Songmu/ghalet"
)

func validate(config string, outStream io.Writer) error {
	if _, err := ghalet.LoadConfig(config); err != nil {
		return err
	}
	_, err := fmt.Fprintln(outStream, "Configuration is valid.")
	return err
}
