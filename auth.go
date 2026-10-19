package ghalet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

const apiTimeout = 45 * time.Second
const preparationTimeout = 10 * time.Minute
const commandTimeout = 15 * time.Second
const maxAttempts = 4

type secrets struct {
	mu     sync.RWMutex
	values []string
	refs   map[string]int
}

func secretComponents(value string) []string {
	if value == "" {
		return nil
	}
	values := []string{value}
	if strings.Contains(value, "\n") {
		for line := range strings.SplitSeq(value, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) != "" {
				values = append(values, line)
			}
		}
	}
	return values
}

func (s *secrets) add(value string) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs == nil {
		s.refs = map[string]int{}
	}
	for _, component := range secretComponents(value) {
		if s.refs[component] == 0 {
			s.values = append(s.values, component)
		}
		s.refs[component]++
	}
}

func (s *secrets) redact(value string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, secret := range s.values {
		value = strings.ReplaceAll(value, secret, "***")
	}
	return value
}

func (s *secrets) addJIT(value string) []string {
	values := []string{value}
	defer func() {
		for _, value := range values {
			s.add(value)
		}
	}()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return values
	}
	values = append(values, string(raw))
	var fields map[string]string
	if json.Unmarshal(raw, &fields) == nil {
		for _, field := range fields {
			values = append(values, field)
			if decoded, err := base64.StdEncoding.DecodeString(field); err == nil {
				values = append(values, string(decoded))
			}
		}
	}
	return values
}

func (s *secrets) release(values []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range values {
		for _, component := range secretComponents(value) {
			if s.refs[component] > 1 {
				s.refs[component]--
				continue
			}
			delete(s.refs, component)
			for i, old := range s.values {
				if old == component {
					copy(s.values[i:], s.values[i+1:])
					s.values[len(s.values)-1] = ""
					s.values = s.values[:len(s.values)-1]
					break
				}
			}
		}
	}
}

func (s *secrets) replaceAttr(_ []string, attr slog.Attr) slog.Attr {
	if attr.Value.Kind() == slog.KindString {
		attr.Value = slog.StringValue(s.redact(attr.Value.String()))
	} else if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(s.redact(err.Error()))
		}
	}
	return attr
}

func readProtected(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("credential file %s must be a regular file accessible only to its owner (0600)", path)
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

func newScaleClient(ctx context.Context, c *Config, scopeName string, secret *secrets) (*scaleset.Client, error) {
	scope, ok := c.Scopes[scopeName]
	if !ok {
		return nil, fmt.Errorf("unknown scope %q", scopeName)
	}
	credential := c.Credentials[scope.CredentialRef]
	options := []scaleset.HTTPOption{scaleset.WithTimeout(apiTimeout), scaleset.WithRetryMax(0)}
	info := scaleset.SystemInfo{System: "ghalet", Version: version, CommitSHA: revision}
	if credential.Type == "github-app" {
		key, err := readProtected(credential.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("scope %s: %w", scopeName, err)
		}
		secret.add(key)
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: scope.URL, SystemInfo: info,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID: credential.ClientID, InstallationID: scope.InstallationID, PrivateKey: key,
			},
		}, options...)
	}
	var token string
	var err error
	switch credential.Type {
	case "gh":
		u, _ := scopeURL(scope.URL)
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
			return nil, fmt.Errorf("scope %s: gh auth token failed (%w); log in on %s and grant runner administration permissions", scopeName, runErr, host)
		}
		token = strings.TrimSpace(string(data))
	case "pat":
		if credential.TokenEnv != "" {
			token = strings.TrimSpace(os.Getenv(credential.TokenEnv))
		} else {
			token, err = readProtected(credential.TokenFile)
		}
	default:
		return nil, errors.New("unsupported credential type")
	}
	if err != nil {
		return nil, fmt.Errorf("scope %s: %w", scopeName, err)
	}
	if token == "" {
		return nil, fmt.Errorf("scope %s: empty management token", scopeName)
	}
	secret.add(token)
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: scope.URL, PersonalAccessToken: token, SystemInfo: info,
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

func runnerGroupName(name string) string {
	if name == "" {
		return scaleset.DefaultRunnerGroup
	}
	return name
}

func registration(ctx context.Context, client scaleAPI, target Target, create bool) (*scaleset.RunnerScaleSet, error) {
	groupName := runnerGroupName(target.RunnerGroup)
	group, err := client.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return nil, fmt.Errorf("reference existing runner group %q (requires runner administration permission): %w", groupName, err)
	}
	if group == nil || group.ID <= 0 {
		return nil, fmt.Errorf("runner group %q does not exist or is inaccessible", groupName)
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
