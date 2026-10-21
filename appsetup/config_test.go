package appsetup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
	"github.com/Songmu/ghalet/credentialstore"
	"github.com/Songmu/ghalet/fshelper"
)

func TestScopeTargetSelection(t *testing.T) {
	for _, tc := range []struct {
		name, scope, id, targets, want, failure string
	}{
		{name: "repository", scope: "Owner/Repo", targets: "[]", want: "owner-repo"},
		{name: "organization", scope: "Owner", targets: "[]", want: "owner"},
		{name: "repository dots", scope: "Owner/Repo.Name", targets: "[]", want: "owner-repo-name"},
		{name: "custom ID", scope: "owner/repo", id: "build", targets: "[]", want: "build"},
		{name: "reuse scope", scope: "OWNER/REPO", targets: "\n  - id: existing\n    scope: https://github.com/owner/repo/", want: "existing"},
		{name: "explicit existing", scope: "owner/repo", id: "existing", targets: "\n  - id: existing\n    scope: owner/repo", want: "existing"},
		{name: "append", scope: "owner/repo", targets: "\n  - id: other\n    scope: owner/other", want: "owner-repo"},
		{name: "ambiguous scope", scope: "owner/repo", targets: "\n  - id: first\n    scope: owner/repo\n  - id: second\n    scope: owner/repo", failure: "multiple targets"},
		{name: "explicit disambiguation", scope: "owner/repo", id: "second", targets: "\n  - id: first\n    scope: owner/repo\n  - id: second\n    scope: owner/repo", want: "second"},
		{name: "explicit additional target", scope: "owner/repo", id: "second", targets: "\n  - id: first\n    scope: owner/repo", want: "second"},
		{name: "ID collision", scope: "owner/repo", targets: "\n  - id: owner-repo\n    scope: owner/other", failure: "collides"},
		{name: "explicit ID collision", scope: "owner/repo", id: "existing", targets: "\n  - id: existing\n    scope: owner/other", failure: "different scope"},
		{name: "long derived ID", scope: "owner/" + strings.Repeat("r", 70), targets: "[]", failure: "specify --id"},
		{name: "long scope explicit ID", scope: "owner/" + strings.Repeat("r", 70), id: "short", targets: "[]", want: "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("STATE_DIRECTORY", "")
			t.Setenv("XDG_STATE_HOME", root)
			t.Setenv("XDG_CACHE_HOME", root)
			path := filepath.Join(root, "config.yaml")
			data := []byte("# keep\ncapacity:\n  maxRunners: 3\ntargets: " + tc.targets + "\n")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			prepared, err := setupConfig(Options{Config: path, Scope: tc.scope, ID: tc.id})
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("got %v; want %q", err, tc.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if prepared.options.ID != tc.want || prepared.target.ID != tc.want || !bytes.Equal(prepared.before, data) ||
				!bytes.Contains(prepared.proposed, []byte("# keep")) {
				t.Fatal("unexpected target selection", prepared.options, prepared.target)
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, data) {
				t.Fatal("setup selection changed configuration before confirmation", err)
			}
		})
	}
}

func TestAppendTargetDoesNotInheritAppInstallation(t *testing.T) {
	f := setupFixture(t, "owner/repo", "User")
	data := []byte("defaults:\n  credential:\n    type: github-app\n    clientId: existing\n    privateKeyFile: existing.pem\ntargets:\n  - id: existing\n    scope: owner/other\n    installationId: 42\n")
	if err := os.WriteFile(f.options.Config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.options.ID = ""
	prepared, err := setupConfig(f.options)
	if err != nil || prepared.target.ID != "owner-repo" || prepared.target.Credential.Type != "gh" {
		t.Fatal("new target inherited an unrelated App installation", err)
	}
	config, err := settings.ParseConfig(f.options.Config, prepared.proposed)
	if err != nil || config.Targets[0].InstallationID != 42 || config.Defaults.Credential.ClientID != "existing" {
		t.Fatal("existing App configuration changed", err)
	}
}

func TestWriteManagedCoordinatesWriters(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "managed.yaml")
	before := []byte("version: 1\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "layout")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	alias, err := settings.ManagedPath(filepath.Join(directory, "config.yaml"), filepath.Join(link, "managed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	after := append(bytes.Clone(before), []byte("# updated\n")...)
	lock, err := fshelper.LockFile(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := settings.WriteManaged(alias, before, after); err == nil {
		t.Fatal("setup ignored another managed writer's lock")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatal("locked managed file was modified", err)
	}
	lock.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- settings.WriteManaged(alias, before, after)
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent writers reported %d successes; want one", successes)
	}
}

func TestSetupRejectsManagedPersistenceCollisionsBeforeAppCreation(t *testing.T) {
	for _, systemd := range []bool{false, true} {
		names := []string{"key", "backup", "metadata", "setup-lock", "managed-lock"}
		if systemd {
			names = append(names, "drop-in")
		}
		for _, name := range names {
			t.Run(fmt.Sprintf("%t/%s", systemd, name), func(t *testing.T) {
				f := setupFixture(t, "owner/repo", "User")
				f.options.Systemd = systemd
				directory, err := settings.StateDirectory()
				if err != nil {
					t.Fatal(err)
				}
				directory = filepath.Join(directory, "credentials")
				files := setupPaths(directory, f.options)
				path := files.diskKey
				switch name {
				case "backup":
					path += ".previous"
				case "metadata":
					path = files.metadata
				case "setup-lock":
					path = filepath.Join(directory, "build.setup.lock")
				case "managed-lock":
					path = filepath.Join(directory, "build.setup")
				case "drop-in":
					path = filepath.Join(credentialstore.SystemdDropInDirectory, "credential-build.conf")
				}
				static := []byte(fmt.Sprintf("managedFile: %s\ntargets:\n  - id: build\n    scope: owner/repo\n", path))
				if err := os.WriteFile(f.options.Config, static, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := f.run(t.Context(), f.options, nil, io.Discard); err == nil {
					t.Fatal("managed persistence collision accepted")
				}
				if f.ownerCalls.Load() != 0 || f.conversions.Load() != 0 {
					t.Fatal("collision was detected only after contacting GitHub")
				}
				if _, err := os.Stat(files.diskKey); !os.IsNotExist(err) {
					t.Fatal("collision preflight created an App key", err)
				}
			})
		}
	}
}
