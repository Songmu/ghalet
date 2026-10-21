package config

import (
	"strings"
	"testing"
)

func TestTargetKey(t *testing.T) {
	base, err := TargetKey("owner", "", "build")
	if err != nil || len(base) != 64 || !ValidTargetID(base) {
		t.Fatal("registration did not produce a bounded, filesystem-safe key", base, err)
	}
	for _, scope := range []string{"owner", "OWNER/", "https://GITHUB.COM/Owner/"} {
		key, err := TargetKey(scope, RunnerGroupName(""), "build")
		if err != nil || key != base {
			t.Fatal("equivalent registrations have different keys", scope, key, err)
		}
	}
	seen := map[string]bool{base: true}
	for _, registration := range [][3]string{
		{"owner/repo", "", "build"},
		{"https://ghe.example.com/owner", "", "build"},
		{"owner", "linux", "build"},
		{"owner", "", "Build"},
		{"owner", "", "build/linux"},
		{"owner", "a", "b/c"},
		{"owner", "a/b", "c"},
		{"owner/" + strings.Repeat("r", 100), "", strings.Repeat("s", 100)},
	} {
		key, err := TargetKey(registration[0], registration[1], registration[2])
		if err != nil || seen[key] || !ValidTargetID(key) {
			t.Fatal("distinct registration collided or produced an unsafe key", registration, key, err)
		}
		seen[key] = true
	}
	for _, registration := range [][3]string{
		{"owner", "", ""},
		{"owner", "", " build"},
		{"owner", "", "build\n"},
		{"owner", "a\x00b", "c"},
		{"owner", "a", "b\x00c"},
		{"owner/repo", "linux", "build"},
		{"http://github.com/owner", "", "build"},
	} {
		if _, err := TargetKey(registration[0], registration[1], registration[2]); err == nil {
			t.Fatal("invalid registration accepted", registration)
		}
	}
}
