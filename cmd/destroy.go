package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"github.com/Songmu/ghalet"
	"github.com/Songmu/prompter"
)

func destroy(ctx context.Context, config, target, runnerGroup, scaleSet string, outStream io.Writer, logger *slog.Logger) error {
	terminal := exec.Command("/bin/sh", "-c", "test -t 0")
	terminal.Stdin = os.Stdin
	if terminal.Run() != nil {
		return errors.New("destroy requires an interactive terminal and explicit yes confirmation; nothing was deleted")
	}
	result, err := ghalet.Destroy(ctx, config, ghalet.DestroyOptions{
		Target: target, RunnerGroup: runnerGroup, ScaleSet: scaleSet,
	}, confirmDestroy, logger)
	return printResponse(outStream, commandResponse{Status: result.Status, Results: result.Results}, err)
}

func confirmDestroy(plan ghalet.DestroyPlan) error {
	prompt := &prompter.Prompter{
		Message: fmt.Sprintf("Delete scale set %q (id %d) at %s, group %q? Type yes to drain and delete",
			plan.ScaleSet, plan.ScaleSetID, plan.URL, plan.RunnerGroup),
	}
	if prompt.Prompt() != "yes" {
		return errors.New("deletion not approved; nothing was deleted")
	}
	return nil
}
