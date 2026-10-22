package ghalet

import (
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
)

func TestSharedCredentialDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, defaults, override string
		want                     settings.Credential
	}{
		{
			name: "shared PAT", defaults: "type: pat\n    tokenEnv: SHARED_PAT",
			want: settings.Credential{Type: "pat", TokenEnv: "SHARED_PAT"},
		},
		{
			name: "explicit gh replaces PAT", defaults: "type: pat\n    tokenEnv: SHARED_PAT",
			override: "    credential:\n      type: gh\n", want: settings.Credential{Type: "gh"},
		},
		{
			name: "empty mapping replaces PAT", defaults: "type: pat\n    tokenEnv: SHARED_PAT",
			override: "    credential: {}\n", want: settings.Credential{Type: "gh"},
		},
		{
			name: "other PAT replaces shared PAT", defaults: "type: pat\n    tokenEnv: SHARED_PAT",
			override: "    credential:\n      type: pat\n      tokenEnv: OTHER_PAT\n",
			want:     settings.Credential{Type: "pat", TokenEnv: "OTHER_PAT"},
		},
		{
			name: "shared App", defaults: "type: github-app\n    clientId: app-client\n    privateKeyFile: /protected/app.pem\n    installationId: 123",
			want: settings.Credential{Type: "github-app", ClientID: "app-client", PrivateKeyFile: "/protected/app.pem", InstallationID: 123},
		},
		{
			name: "PAT replaces App", defaults: "type: github-app\n    clientId: app-client\n    privateKeyFile: /protected/app.pem\n    installationId: 123",
			override: "    credential:\n      type: pat\n      tokenEnv: OTHER_PAT\n",
			want:     settings.Credential{Type: "pat", TokenEnv: "OTHER_PAT"},
		},
		{
			name: "implicit gh", defaults: "hostname: github.com",
			want: settings.Credential{Type: "gh", Hostname: "github.com"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "defaults:\n  credential:\n    " + tc.defaults +
				"\ntargets:\n  - scaleSet: one\n    scope: owner/one\n" + tc.override +
				"  - scaleSet: two\n    scope: owner/two\n" + tc.override
			config, err := settings.LoadConfig(writeConfig(t, input))
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range config.Targets {
				if target.Credential != tc.want {
					t.Fatalf("got %+v, want %+v", target.Credential, tc.want)
				}
			}
			config.Targets[0].Credential.Type = "changed"
			if config.Targets[1].Credential.Type == "changed" || config.Defaults.Credential.Type == "changed" {
				t.Fatal("targets share mutable credentials")
			}
		})
	}
}

func TestSharedCredentialPathsAndEnvironment(t *testing.T) {
	path := writeConfig(t, "defaults:\n  credential:\n    type: pat\n    tokenFile: secrets/token\ntargets:\n  - scaleSet: one\n    scope: owner/repository\n")
	config, err := settings.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(path), "secrets/token")
	if config.Defaults.Credential.TokenFile != want || config.Targets[0].Credential.TokenFile != want {
		t.Fatal("shared paths were not resolved against config directory")
	}
	input := "defaults:\n  credential:\n    type: pat\n    tokenEnv: SHARED_PAT\ntargets:\n  - scaleSet: one\n    scope: owner/repository\n    credential:\n      type: gh\nrunner:\n  env:\n    SHARED_PAT: do-not-copy\n"
	if _, err := settings.LoadConfig(writeConfig(t, input)); err == nil || !strings.Contains(err.Error(), "management credential") {
		t.Fatal("default credential environment was not protected", err)
	}
}

func TestInvalidSharedCredentials(t *testing.T) {
	for _, input := range []string{
		"defaults:\n  credential:\n    type: pat\n",
		"defaults:\n  credential:\n    type: pat\n    tokenEnv: TOKEN\n    privateKeyFile: key.pem\n",
		"defaults:\n  credential:\n    type: \"\"\n",
		"defaults:\n  credential:\n    type: github-app\n    clientId: app-client\n    privateKeyFile: key.pem\n",
	} {
		input += "targets:\n  - scaleSet: one\n    scope: owner/repository\n"
		if _, err := settings.LoadConfig(writeConfig(t, input)); err == nil {
			t.Fatal("invalid defaults were accepted", input)
		}
	}
	input := "defaults:\n  credential:\n    type: pat\ntargets:\n  - scaleSet: one\n    scope: owner/repository\n    credential:\n      type: gh\n"
	if _, err := settings.LoadConfig(writeConfig(t, input)); err == nil || !strings.Contains(err.Error(), "defaults.credential") {
		t.Fatal("unused invalid defaults were silently ignored", err)
	}
}
