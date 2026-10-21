package appsetup

import (
	"strings"
	"testing"
)

func TestNormalizeScope(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Songmu/ghalet", "Songmu/ghalet"},
		{"Songmu", "Songmu"},
		{"https://github.com/Songmu/ghalet/", "Songmu/ghalet"},
		{"https://GITHUB.COM/Songmu", "Songmu"},
		{"owner/repo.name", "owner/repo.name"},
	} {
		got, err := normalizeScope(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, input := range []string{
		"", "owner/repo/extra", "owner//", "/owner/repo", "owner/..", "owner/.",
		"https://github.example/owner/repo", "http://github.com/owner/repo",
		"https://user@github.com/owner/repo", "https://github.com:443/owner/repo",
		"https://github.com/owner/repo?query", "https://github.com/owner/repo?",
		"https://github.com/owner/repo#fragment", "https://github.com/owner/re%70o",
		"owner/repo name", "owner\\repo", "enterprises/example", "owner/日本語",
	} {
		if got, err := normalizeScope(input); err == nil {
			t.Fatalf("accepted %q as %q", input, got)
		}
	}
}

func TestOptionsRequireScope(t *testing.T) {
	if err := (Options{ScaleSet: "build"}).Validate(); err == nil || !strings.Contains(err.Error(), "--scope is required") {
		t.Fatal(err)
	}
}
