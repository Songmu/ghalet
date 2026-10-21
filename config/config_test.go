package config

import "testing"

func TestRetainSecretEnvironment(t *testing.T) {
	old := &Config{
		Defaults: DefaultsConfig{Credential: &Credential{TokenEnv: "DEFAULT_PAT"}},
		Targets:  []Target{{Credential: Credential{TokenEnv: "TARGET_PAT"}}},
	}
	current := &Config{Targets: []Target{{Credential: Credential{TokenEnv: "CURRENT_PAT"}}}}
	current.RetainSecretEnvironment(old)
	next := &Config{}
	next.RetainSecretEnvironment(current)
	for _, name := range []string{"DEFAULT_PAT", "TARGET_PAT", "CURRENT_PAT", "GH_TOKEN", "github_token"} {
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
