package ghalet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/logreduct"
	"github.com/actions/scaleset"
)

const apiTimeout = 45 * time.Second
const preparationTimeout = 10 * time.Minute
const commandTimeout = 15 * time.Second
const maxAttempts = 4

func readProtected(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("%w: credential file %s must be a regular file accessible only to its owner (0600)", errUnsafeTarget, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("credential file %s is empty", path)
	}
	return strings.TrimSpace(string(data)), nil
}

func newScaleClient(ctx context.Context, target settings.Target, secret *logreduct.Redactor) (*scaleset.Client, error) {
	credential := target.Credential
	options := []scaleset.HTTPOption{scaleset.WithTimeout(apiTimeout), scaleset.WithRetryMax(0)}
	info := scaleset.SystemInfo{System: "ghalet", Version: Version, CommitSHA: Revision}
	if credential.Type == "github-app" {
		key, err := readProtected(credential.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", target.ID, err)
		}
		secret.Add(key)
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: target.URL, SystemInfo: info,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID: credential.ClientID, InstallationID: target.InstallationID, PrivateKey: key,
			},
		}, options...)
	}
	var token string
	var err error
	switch credential.Type {
	case "gh":
		u, _ := settings.ScopeURL(target.URL)
		host := credential.Hostname
		if host == "" {
			host = u.Host
		}
		commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		command := exec.CommandContext(commandCtx, "gh", "auth", "token", "--hostname", host)
		command.WaitDelay = time.Second
		data, runErr := command.Output()
		if runErr != nil {
			// gh's stderr can include credentials or user-controlled text.
			return nil, fmt.Errorf("target %s: gh auth token failed (%w); log in on %s and grant runner administration permissions", target.ID, runErr, host)
		}
		token = strings.TrimSpace(string(data))
	case "pat":
		if credential.TokenEnv != "" {
			token = strings.TrimSpace(os.Getenv(credential.TokenEnv))
		} else {
			token, err = readProtected(credential.TokenFile)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported credential type", errUnsafeTarget)
	}
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", target.ID, err)
	}
	if token == "" {
		return nil, fmt.Errorf("%w: target %s: empty management token", errUnsafeTarget, target.ID)
	}
	secret.Add(token)
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: target.URL, PersonalAccessToken: token, SystemInfo: info,
	}, options...)
}

type scaleAPI interface {
	GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
	GetRunnerScaleSetByID(context.Context, int) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(context.Context, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(context.Context, int) error
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunner(context.Context, int) (*scaleset.RunnerReference, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

func registration(ctx context.Context, client scaleAPI, target settings.Target, create bool) (*scaleset.RunnerScaleSet, error) {
	groupName := settings.RunnerGroupName(target.RunnerGroup)
	group, err := client.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("reference existing runner group %q (requires runner administration permission): %w", groupName, err)
	}
	if group == nil || group.ID <= 0 {
		return nil, fmt.Errorf("%w: runner group %q does not exist or is inaccessible", errUnsafeTarget, groupName)
	}
	set, err := client.GetRunnerScaleSet(ctx, group.ID, target.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("get scale set %q (repository administration or organization self-hosted runner permission required): %w", target.ScaleSetName, err)
	}
	if !create {
		return set, nil
	}

	if set == nil {
		return client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: target.ScaleSetName, RunnerGroupID: group.ID,
			Labels:        []scaleset.Label{{Name: target.ScaleSetName, Type: "System"}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
	}
	if !set.RunnerSetting.DisableUpdate {
		set.RunnerSetting.DisableUpdate = true
		return client.UpdateRunnerScaleSet(ctx, set.ID, set)
	}
	return set, nil
}
