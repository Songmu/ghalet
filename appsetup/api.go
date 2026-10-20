package appsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type apiClient struct {
	base string
	http *http.Client
}

type httpError int

func (e httpError) Error() string { return fmt.Sprintf("GitHub API returned HTTP %d", e) }

func (api apiClient) request(ctx context.Context, method, path, token string, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, api.base+path, nil)
	if err != nil {
		return errors.New("cannot construct GitHub API request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := api.http.Do(req)
	if err != nil {
		// The request URL can contain the one-time Manifest code.
		var requestError *url.Error
		if errors.As(err, &requestError) {
			return fmt.Errorf("GitHub API transport failed: %w", requestError.Err)
		}
		return errors.New("GitHub API transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return httpError(response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		clear(data)
		return errors.New("cannot read GitHub API response")
	}
	defer clear(data)
	if len(data) > 1024*1024 {
		return errors.New("GitHub API response exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid GitHub API response")
	}
	return nil
}

type account struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type app struct {
	ID       int64   `json:"id"`
	ClientID string  `json:"client_id"`
	Slug     string  `json:"slug"`
	Owner    account `json:"owner"`
	PEM      string  `json:"pem"`
}

type installation struct {
	ID          int64             `json:"id"`
	AppID       int64             `json:"app_id"`
	Account     account           `json:"account"`
	Permissions map[string]string `json:"permissions"`
	SuspendedAt *string           `json:"suspended_at"`
}

func appToken(clientID string, pem []byte) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return "", errors.New("saved App key is not an RSA private key")
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: clientID, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	})
	return token.SignedString(key)
}

func installationPath(scope string) string {
	parts := strings.Split(scope, "/")
	if len(parts) == 1 {
		return "/orgs/" + url.PathEscape(parts[0]) + "/installation"
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/installation"
}
