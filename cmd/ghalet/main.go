package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"

	"github.com/Songmu/ghalet/cmd"
)

func main() {
	syscall.Umask(0077)
	err := cmd.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && err != flag.ErrHelp {
		fmt.Fprintln(os.Stderr, err)
		exitCode := 1
		if ecoder, ok := err.(interface{ ExitCode() int }); ok {
			exitCode = ecoder.ExitCode()
		}
		os.Exit(exitCode)
	}
}
