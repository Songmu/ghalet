package cmd

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestAuthHelp(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "--help"}, {"auth", "setup", "--help"},
		{"--config", "custom.yaml", "auth", "setup", "--help"},
	} {
		var output bytes.Buffer
		if err := Run(t.Context(), args, &output, &output); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "ghalet auth setup") {
			t.Fatal(output.String())
		}
	}
}

func TestAuthOptions(t *testing.T) {
	options, err := parseAuthOptions([]string{"work", "--target", "build", "--listen", "127.0.0.1:8765", "--no-browser", "--yes"}, "selected.yaml", &bytes.Buffer{})
	if err != nil || options.Name != "work" || options.Target != "build" || options.Config != "selected.yaml" ||
		options.Listen != "127.0.0.1:8765" || !options.NoBrowser || !options.Yes {
		t.Fatal(options, err)
	}
	for _, args := range [][]string{{}, {"work", "extra"}, {"--yes"}} {
		if _, err := parseAuthOptions(args, "", &bytes.Buffer{}); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}
