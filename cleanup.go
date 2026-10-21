package ghalet

import (
	"context"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/runnerlog"
)

func expireLogs(ctx context.Context, c *settings.Config, dryRun bool) ([]OperationResult, error) {
	expired, err := runnerlog.Expire(ctx, c.Logs.Root, time.Duration(c.Logs.Retention), dryRun)
	var results []OperationResult
	for _, result := range expired {
		results = append(results, OperationResult{Path: result.Path, Action: result.Action, Error: result.Error})
	}
	return results, err
}
