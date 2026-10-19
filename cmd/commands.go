package cmd

import (
	"context"
	"io"
)

var commands = map[string]func(context.Context, options, io.Writer, io.Writer) error{
	"run":      run,
	"validate": validate,
	"status":   status,
	"doctor":   doctor,
	"drain":    drain,
	"gc":       gc,
	"destroy":  destroy,
}
