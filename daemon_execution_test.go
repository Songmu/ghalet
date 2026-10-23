package ghalet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/actions/scaleset"
)

type invalidJITAPI struct {
	scaleAPI
	change func(*scaleset.RunnerScaleSetJitRunnerConfig) *scaleset.RunnerScaleSetJitRunnerConfig
}

func (api invalidJITAPI) GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	response, err := api.scaleAPI.GenerateJitRunnerConfig(ctx, setting, id)
	if err != nil {
		return nil, err
	}
	return api.change(response), nil
}

func TestPreparationRejectsIncompleteJITBeforeLaunch(t *testing.T) {
	for _, name := range []string{"nil response", "nil runner", "missing ID", "missing JIT", "wrong name", "wrong scale set"} {
		t.Run(name, func(t *testing.T) {
			d, _ := testDaemon(t)
			state := testState(t, d.config)
			marker := filepath.Join(filepath.Dir(state.RunnerDir), "launched")
			seedDistribution(t, d.config, fmt.Sprintf("#!/bin/sh\nprintf launched > %q\n", marker))
			if err := d.store.save(state); err != nil {
				t.Fatal(err)
			}
			target := d.targets[buildTargetID]
			target.Client = invalidJITAPI{scaleAPI: target.Client, change: func(response *scaleset.RunnerScaleSetJitRunnerConfig) *scaleset.RunnerScaleSetJitRunnerConfig {
				switch name {
				case "nil response":
					return nil
				case "nil runner":
					response.Runner = nil
				case "missing ID":
					response.Runner.ID = 0
				case "missing JIT":
					response.EncodedJITConfig = ""
				case "wrong name":
					response.Runner.Name += "-other"
				case "wrong scale set":
					response.Runner.RunnerScaleSetID++
				}
				return response
			}}
			run := &activeRun{State: state, Config: d.config, Target: target}
			d.runs[state.RunID] = run
			cleanupProcesses(t, d)
			if err := d.prepare(t.Context(), target, run); !errors.Is(err, errUnsafeTarget) {
				t.Fatal("incomplete JIT response was not classified as unsafe", err)
			}
			if state.Phase != "preparing" || state.RunnerID != 0 || state.Process.PID != 0 || run.JITSecrets != nil {
				t.Fatal("invalid JIT response advanced runner launch state", state)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid JIT response launched the runner", err)
			}
		})
	}
}
