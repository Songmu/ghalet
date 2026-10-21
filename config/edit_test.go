package config

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestEditorTargetEdits(t *testing.T) {
	input := "# keep header\ncapacity:\n  maxRunners: 3\ntargets:\n  - id: 123 # keep ID comment\n    scope: owner/old\n    credential: &old\n      type: pat\n      tokenEnv: OLD_TOKEN\n  - id: alias\n    scope: owner/alias\n    credential: *old\n"
	editor, err := NewEditor([]byte(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := editor.SetTargetApp("123", "client", "app.pem", 42); err != nil {
		t.Fatal(err)
	}
	if err := editor.AppendTarget("new", "owner/new"); err != nil {
		t.Fatal(err)
	}
	if err := editor.SetTargetApp("new", "new-client", "new.pem", 43); err != nil {
		t.Fatal("cannot edit an appended target", err)
	}
	data, err := editor.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, comment := range []string{"# keep header", "# keep ID comment"} {
		if !strings.Contains(output, comment) {
			t.Fatalf("lost comment %q:\n%s", comment, data)
		}
	}
	if strings.Index(output, "capacity:") > strings.Index(output, "targets:") {
		t.Fatal("mapping order changed", output)
	}
	var decoded struct {
		Targets []struct {
			ID             string `yaml:"id"`
			InstallationID int64  `yaml:"installationId"`
			Credential     struct {
				Type     string `yaml:"type"`
				TokenEnv string `yaml:"tokenEnv"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Targets) != 3 || decoded.Targets[0].ID != "123" ||
		decoded.Targets[0].InstallationID != 42 || decoded.Targets[0].Credential.Type != "github-app" ||
		decoded.Targets[1].Credential.TokenEnv != "OLD_TOKEN" || decoded.Targets[2].InstallationID != 43 {
		t.Fatalf("target or alias semantics changed: %+v\n%s", decoded, data)
	}
	if err := editor.SetTargetApp("missing", "client", "app.pem", 1); err == nil {
		t.Fatal("missing target accepted")
	}
}

func TestEditorAppendTargetOverridesDefaults(t *testing.T) {
	editor, err := NewEditor([]byte("defaults:\n  credential:\n    type: github-app\n    clientId: existing\n    privateKeyFile: old.pem\ntargets: []\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := editor.AppendTarget("new", "owner/new"); err != nil {
		t.Fatal(err)
	}
	data, err := editor.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "credential: {}") {
		t.Fatal("new target can inherit an unrelated App installation", string(data))
	}
}

func TestEditorRejectsInvalidDocuments(t *testing.T) {
	for _, input := range []string{"", "[]", "targets: []\n---\ntargets: []\n", "targets: ["} {
		if _, err := NewEditor([]byte(input), nil); err == nil {
			t.Fatalf("accepted invalid configuration %q", input)
		}
	}
	var schema struct {
		Targets []targetIdentity `yaml:"targets"`
	}
	if _, err := NewEditor([]byte("unknown: value\ntargets: []\n"), &schema); err == nil {
		t.Fatal("strict schema validation ignored an unknown field")
	}
	for _, input := range []string{"defaults: {}", "targets: {}", "targets: [123]"} {
		editor, err := NewEditor([]byte(input), nil)
		if err == nil {
			err = editor.AppendTarget("new", "owner/new")
		}
		if err == nil {
			t.Fatalf("accepted invalid targets %q", input)
		}
	}
}
