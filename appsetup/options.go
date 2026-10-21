package appsetup

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/Songmu/ghalet/credentialstore"
)

// Options selects a GitHub.com scope, optional target ID and credential backend.
type Options struct {
	Scope, ID, Config, Listen string
	Systemd, NoBrowser, Yes   bool
}

// Validate checks explicit input before creating an App or saving credentials.
func (options Options) Validate() error {
	if _, err := normalizeScope(options.Scope); err != nil {
		return err
	}
	if options.ID != "" {
		return validateID(options.ID)
	}
	return nil
}

func validateID(id string) error {
	return (credentialstore.Options{Name: id, Type: "github-app", Stdin: true, NoDefault: true}).Validate()
}

var scopePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func normalizeScope(scope string) (string, error) {
	if scope == "" {
		return "", errors.New("--scope is required (OWNER/REPO or ORG)")
	}
	if !strings.Contains(scope, "://") {
		scope = "https://github.com/" + scope
	}
	address, err := url.Parse(scope)
	if err != nil || address.Scheme != "https" || !strings.EqualFold(address.Host, "github.com") ||
		address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || address.RawPath != "" {
		return "", errors.New("--scope must be a GitHub.com organization or repository")
	}
	scope = strings.TrimSuffix(strings.TrimPrefix(address.Path, "/"), "/")
	parts := strings.Split(scope, "/")
	if len(parts) < 1 || len(parts) > 2 || strings.EqualFold(parts[0], "enterprises") {
		return "", errors.New("--scope must be OWNER/REPO or ORG")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("invalid --scope path")
		}
		if !scopePart.MatchString(part) {
			return "", errors.New("--scope must use ASCII organization and repository names")
		}
	}
	return scope, nil
}
