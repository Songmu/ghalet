package appsetup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
)

func TestSetupDefaultAddsStaticTarget(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "symlink"}[symlink], func(t *testing.T) {
			f := setupFixture(t, "owner/repo", "User")
			f.options.Target = false
			actual := f.options.Config
			if err := os.WriteFile(actual, []byte("# keep\ntargets: []\n"), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(actual, 0640); err != nil {
				t.Fatal(err)
			}
			if symlink {
				f.options.Config = filepath.Join(filepath.Dir(actual), "linked.yaml")
				if err := os.Symlink(actual, f.options.Config); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.run(t.Context(), f.options, nil, io.Discard); err != nil {
				t.Fatal(err)
			}
			config, err := settings.LoadConfig(f.options.Config)
			if err != nil || len(config.Targets) != 1 || config.Targets[0].Credential.InstallationID != 123 {
				t.Fatal("target was not added with inherited authentication", config, err)
			}
			data, err := os.ReadFile(actual)
			if err != nil || !bytes.Contains(data, []byte("# keep")) || bytes.Contains(data, []byte("credential:")) {
				t.Fatal("static configuration lost comments or gained authentication", err)
			}
			info, err := os.Stat(actual)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("configuration permissions changed", err)
			}
			if symlink {
				info, err := os.Lstat(f.options.Config)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("configuration symlink was replaced", err)
				}
			}
		})
	}
}

func TestSetupReplacesDifferentHostDefault(t *testing.T) {
	for _, storage := range []string{"static", "managed"} {
		for _, target := range []bool{false, true} {
			name := storage + "/overwrite"
			if target {
				name = storage + "/target"
			}
			t.Run(name, func(t *testing.T) {
				f := setupFixture(t, "owner/repo", "User")
				f.options.Target, f.options.Overwrite = target, !target
				defaults := []byte("defaults:\n  credential:\n    type: gh\n    hostname: ghe.example.com\n")
				static := []byte("targets: []\n")
				if storage == "static" {
					static = append(static, defaults...)
				} else {
					path, err := settings.ManagedPath(f.options.Config, "")
					if err != nil {
						t.Fatal(err)
					}
					if err := settings.WriteManaged(path, nil, defaults); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(f.options.Config, static, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.run(t.Context(), f.options, nil, io.Discard); err != nil {
					t.Fatal("unrelated default prevented App setup", err)
				}
				config, err := settings.LoadConfig(f.options.Config)
				if err != nil || len(config.Targets) != 1 || config.Targets[0].Credential.Type != "github-app" ||
					config.Targets[0].Credential.InstallationID != 123 {
					t.Fatal("target did not receive the final App credential", config, err)
				}
				data, err := os.ReadFile(f.options.Config)
				if err != nil || bytes.Contains(data, []byte("type: github-app")) {
					t.Fatal("temporary authentication leaked into static configuration", err)
				}
			})
		}
	}
}

func TestSetupDefaultCredential(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	f.options.Target = false
	input, _ := os.ReadFile(f.options.Config)
	editor, err := settings.NewEditor(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	editor.RemoveDefaultCredential()
	before, err := editor.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.options.Config, before, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	result, err := f.run(t.Context(), f.options, nil, &output)
	if err != nil {
		t.Fatal(err)
	}
	config, err := settings.LoadConfig(f.options.Config)
	if err != nil {
		t.Fatal(err)
	}
	credential := config.Defaults.Credential
	if credential == nil || credential.InstallationID != 123 || credential.PrivateKeyFile != result.KeyPath ||
		config.Targets[0].Credential != *credential || config.Targets[1].Credential.Type != "gh" {
		t.Fatal("default credential was not inherited as a complete credential", config)
	}
	after, err := os.ReadFile(f.options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing target configuration was unnecessarily rewritten", err)
	}
	managed, _, err := settings.ReadManaged(config.ManagedFile)
	if err != nil || len(managed.Targets) != 0 || !strings.Contains(output.String(), "Save defaults.credential") {
		t.Fatal("default saved as target authentication", managed, err)
	}
	if _, err := f.run(t.Context(), f.options, nil, io.Discard); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Fatal("repeated setup replaced defaults without explicit permission", err)
	}
	f.options.Overwrite = true
	if _, err := f.run(t.Context(), f.options, nil, io.Discard); err != nil || f.conversions.Load() != 1 {
		t.Fatal("explicit overwrite did not reuse the saved App", err)
	}
}

func TestSetupExistingDefaults(t *testing.T) {
	for _, storage := range []string{"static", "managed"} {
		for _, selection := range []string{"unspecified", "target", "overwrite"} {
			t.Run(storage+"/"+selection, func(t *testing.T) {
				f := setupFixture(t, "owner/repo", "User")
				f.options.Target = selection == "target"
				f.options.Overwrite = selection == "overwrite"
				before, _ := os.ReadFile(f.options.Config)
				path, err := settings.ManagedPath(f.options.Config, "")
				if err != nil {
					t.Fatal(err)
				}
				if storage == "managed" {
					editor, err := settings.NewEditor(before, nil)
					if err != nil {
						t.Fatal(err)
					}
					editor.RemoveDefaultCredential()
					before, err = editor.Marshal()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.options.Config, before, 0600); err != nil {
						t.Fatal(err)
					}
					if err := settings.WriteManaged(path, nil, []byte("defaults:\n  credential:\n    type: gh\n")); err != nil {
						t.Fatal(err)
					}
				}
				_, err = f.run(t.Context(), f.options, nil, io.Discard)
				if selection == "unspecified" {
					if err == nil || !strings.Contains(err.Error(), "--target") || !strings.Contains(err.Error(), "--overwrite") ||
						f.ownerCalls.Load() != 0 || f.conversions.Load() != 0 {
						t.Fatal("default conflict was not rejected before creating an App", err)
					}
					after, _ := os.ReadFile(f.options.Config)
					if !bytes.Equal(before, after) {
						t.Fatal("rejected setup changed static configuration")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				config, err := settings.LoadConfig(f.options.Config)
				if err != nil {
					t.Fatal(err)
				}
				wantDefault := "gh"
				if selection == "overwrite" {
					wantDefault = "github-app"
				}
				if config.Defaults.Credential.Type != wantDefault || config.Targets[0].Credential.InstallationID != 123 ||
					config.Targets[1].Credential.Type != "gh" || len(config.Targets) != 2 {
					t.Fatal("setup changed unrelated targets or did not apply the selected credential", config)
				}
			})
		}
	}
}
