package cmd

import (
	"fmt"
	"io"

	settings "github.com/Songmu/ghalet/config"
)

func validate(config string, outStream io.Writer) error {
	if _, err := settings.LoadConfig(config); err != nil {
		return err
	}
	_, err := fmt.Fprintln(outStream, "Configuration is valid.")
	return err
}
