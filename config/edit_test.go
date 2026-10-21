package config

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestEditorTargetEdits(t *testing.T) {
	input := "# keep header\nunknown: keep\ncapacity:\n  maxRunners: 3\n  unknown: nested\ntargets:\n  - scaleSet: 123 # keep scale set comment\n    scope: owner/old\n    unknown: target\n    credential: &old\n      type: pat\n      tokenEnv: OLD_TOKEN\n  - scaleSet: alias\n    scope: owner/alias\n    credential: *old\n"
	editor, err := NewEditor([]byte(input), &Config{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := TargetKey("owner/old", "", "123")
	if err := editor.SetTargetApp(first, "client", "app.pem", 42); err != nil {
		t.Fatal(err)
	}
	if err := editor.AppendTarget("owner/new", "", "new"); err != nil {
		t.Fatal(err)
	}
	added, _ := TargetKey("owner/new", "", "new")
	if err := editor.SetTargetApp(added, "new-client", "new.pem", 43); err != nil {
		t.Fatal("cannot edit an appended target", err)
	}
	data, err := editor.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, preserved := range []string{"# keep header", "# keep scale set comment", "unknown: keep", "unknown: nested", "unknown: target"} {
		if !strings.Contains(output, preserved) {
			t.Fatalf("lost content %q:\n%s", preserved, data)
		}
	}
	if strings.Index(output, "capacity:") > strings.Index(output, "targets:") {
		t.Fatal("mapping order changed", output)
	}
	var decoded struct {
		Targets []struct {
			ScaleSet       string `yaml:"scaleSet"`
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
	if len(decoded.Targets) != 3 || decoded.Targets[0].ScaleSet != "123" ||
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
	if err := editor.AppendTarget("owner/new", "", "new"); err != nil {
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
	if _, err := NewEditor([]byte("targets: invalid\n"), &schema); err == nil {
		t.Fatal("schema validation ignored an invalid field type")
	}
	for _, input := range []string{"defaults: {}", "targets: {}", "targets: [123]"} {
		editor, err := NewEditor([]byte(input), nil)
		if err == nil {
			err = editor.AppendTarget("owner/new", "", "new")
		}
		if err == nil {
			t.Fatalf("accepted invalid targets %q", input)
		}
	}
}
