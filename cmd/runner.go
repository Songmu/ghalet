package cmd

import (
	"errors"

	"github.com/Songmu/ghalet"
)

func runnerUser(argv []string) error {
	if len(argv) > 1 || (len(argv) == 1 && argv[0] == "") {
		return errors.New("usage: ghalet __ghalet_runner_user [runner]")
	}
	runner := ""
	if len(argv) == 1 {
		runner = argv[0]
	}
	return ghalet.RunRunnerUser(runner)
}

func runnerGuard(argv []string) error {
	if len(argv) != 1 || argv[0] == "" {
		return errors.New("usage: ghalet __ghalet_runner_guard runner")
	}
	return ghalet.RunRunnerGuard(argv[0])
}
