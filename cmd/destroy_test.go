package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Songmu/ghalet"
)

func TestConfirmDestroy(t *testing.T) {
	if os.Getenv("GHALET_TEST_CONFIRM_HELPER") == "1" {
		plan := ghalet.DestroyPlan{
			Target: "build", URL: "https://github.com/owner/repository", ScaleSet: "build", ScaleSetID: 10,
		}
		if err := confirmDestroy(plan); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, answer := range []string{"yes\n", "no\n", "y\n", "\n", "YES\n", ""} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestConfirmDestroy$")
			command.Env = append(os.Environ(), "GHALET_TEST_CONFIRM_HELPER=1", "GO_PROMPTER_USE_DEFAULT=", "GO_PROMPTER_USE_PIPE=1")
			command.Stdin = strings.NewReader(answer)
			var output, stderr bytes.Buffer
			command.Stdout, command.Stderr = &output, &stderr
			err := command.Run()
			if (err == nil) != (answer == "yes\n") {
				t.Fatalf("answer %q: %v (%s)", answer, err, stderr.String())
			}
			if output.Len() != 0 {
				t.Fatalf("prompt contaminated stdout: %s", output.String())
			}
			if !strings.Contains(stderr.String(), `Delete scale set "build" (id 10)`) {
				t.Fatalf("missing confirmation details on stderr: %s", stderr.String())
			}
			if answer == "" && !strings.Contains(stderr.String(), "confirm destruction: read input: EOF") {
				t.Fatalf("prompt read failure was not propagated: %s", stderr.String())
			}
		})
	}
}
