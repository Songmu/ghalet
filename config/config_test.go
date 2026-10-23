package config

import "testing"

func TestScopeURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantErr   bool
	}{
		{"organization", "https://github.com/org", false},
		{"repository", "https://github.com/org/repo", false},
		{"enterprise host", "https://ghe.example.com/org/repo", false},
		{"missing owner", "https://github.com", true},
		{"parent traversal", "https://github.com/a/../b", true},
		{"extra path", "https://github.com/a/b/c", true},
		{"encoded slash", "https://github.com/a%2fb", true},
		{"explicit port", "https://github.com:443/org", true},
		{"enterprise scope", "https://github.com/Enterprises/acme", true},
		{"uppercase enterprise scope", "https://github.com/ENTERPRISES/acme", true},
		{"empty organization", "https://github.com//org", true},
		{"empty repository owner", "https://github.com//org/repo", true},
		{"missing authority", "http:github.com/org", true},
		{"HTTP", "http://github.com/org", true},
		{"SSH", "ssh://github.com/org", true},
		{"userinfo", "https://user:secret@github.com/org", true},
		{"query", "https://github.com/org?token=hidden", true},
		{"fragment", "https://github.com/org#fragment", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ScopeURL(tc.raw); (err != nil) != tc.wantErr {
				t.Fatalf("ScopeURL(%q): got %v, want error=%v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestRetainSecretEnvironment(t *testing.T) {
	old := &Config{
		Defaults: DefaultsConfig{Credential: &Credential{TokenEnv: "DEFAULT_PAT"}},
		Targets:  []Target{{Credential: Credential{TokenEnv: "TARGET_PAT"}}},
	}
	current := &Config{Targets: []Target{{Credential: Credential{TokenEnv: "CURRENT_PAT"}}}}
	current.RetainSecretEnvironment(old)
	next := &Config{}
	next.RetainSecretEnvironment(current)
	for _, name := range []string{
		"DEFAULT_PAT", "TARGET_PAT", "CURRENT_PAT", "GH_TOKEN", "github_token",
		"OTEL_EXPORTER_OTLP_HEADERS", "otel_exporter_otlp_metrics_headers",
	} {
		if !next.SecretEnv(name) {
			t.Errorf("lost secret environment name %q", name)
		}
		next.Runner.Env = map[string]string{name: "value"}
		if err := next.ValidateEnvironment(); err == nil {
			t.Errorf("accepted historical secret override %q", name)
		}
	}
	if next.SecretEnv("") || next.SecretEnv("BUILD_OPTION") {
		t.Fatal("ordinary environment names treated as credentials")
	}
	next.Runner.Env = map[string]string{"BUILD_OPTION": "value"}
	if err := next.ValidateEnvironment(); err != nil {
		t.Fatal(err)
	}
	next.secretNames["ISOLATED"] = true
	if current.SecretEnv("ISOLATED") {
		t.Fatal("secret history is shared between configurations")
	}
}
