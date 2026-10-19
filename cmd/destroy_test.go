package cmd

import (
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
			os.Exit(1)
		}
		os.Exit(0)
	}
	for _, answer := range []string{"yes\n", "no\n", "y\n", "\n", "YES\n", ""} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestConfirmDestroy$")
			command.Env = append(os.Environ(), "GHALET_TEST_CONFIRM_HELPER=1", "GO_PROMPTER_USE_DEFAULT=", "GO_PROMPTER_USE_PIPE=1")
			command.Stdin = strings.NewReader(answer)
			output, err := command.CombinedOutput()
			if (err == nil) != (answer == "yes\n") {
				t.Fatalf("answer %q: %v (%s)", answer, err, output)
			}
			if !strings.Contains(string(output), `Delete scale set "build" (id 10)`) {
				t.Fatalf("missing confirmation details: %s", output)
			}
		})
	}
}
