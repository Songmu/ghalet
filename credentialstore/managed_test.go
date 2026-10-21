package credentialstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	settings "github.com/Songmu/ghalet/config"
)

func TestManagedCredentialUpdatesRejectConflicts(t *testing.T) {
	for _, backend := range []string{"file", "systemd"} {
		for _, kind := range []string{"concurrent", "credential", "backup", "lock"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				store, options, tools := testCredentialStore(t)
				set := store.set
				extension := ".cred"
				if backend == "file" {
					extension = ".pat"
					set = func(ctx context.Context, options Options, input io.Reader) (Result, error) {
						return setFile(ctx, store.directory, options, input)
					}
				}
				if kind == "concurrent" {
					first, err := set(t.Context(), options, strings.NewReader("old-secret"))
					if err != nil {
						t.Fatal(err)
					}
					secretPath := first.RuntimePath
					if backend == "systemd" {
						secretPath = first.EncryptedPath
					}
					original, err := os.ReadFile(secretPath)
					if err != nil {
						t.Fatal(err)
					}
					edit := func() {
						if err := os.WriteFile(first.ManagedPath, []byte("version: 1\n# concurrent\n"), 0600); err != nil {
							t.Error(err)
						}
					}
					var input io.Reader = strings.NewReader("replacement-secret")
					if backend == "systemd" {
						tools.onEncrypt = edit
					} else {
						input = &onRead{Reader: input, run: edit}
					}
					if _, err := set(t.Context(), options, input); err == nil {
						t.Fatal("concurrent managed update overwritten")
					}
					current, err := os.ReadFile(secretPath)
					if err != nil || !bytes.Equal(current, original) {
						t.Fatal("concurrent managed edit changed existing secret", err)
					}
					managed, err := os.ReadFile(first.ManagedPath)
					if err != nil || string(managed) != "version: 1\n# concurrent\n" {
						t.Fatal("concurrent managed edit was not preserved", err)
					}
					return
				}
				dest := filepath.Join(store.directory, options.Name+extension)
				if kind == "backup" {
					dest += ".previous"
				} else if kind == "lock" {
					dest = filepath.Join(store.directory, ".lock")
				}
				static := []byte("managedFile: " + dest + "\ntargets: []\n")
				if err := os.WriteFile(options.Config, static, 0600); err != nil {
					t.Fatal(err)
				}
				input := strings.NewReader("synthetic-secret")
				if _, err := set(t.Context(), options, input); err == nil || input.Len() != len("synthetic-secret") {
					t.Fatal("managed collision was not rejected before input", err)
				}
			})
		}
	}
}

type onRead struct {
	io.Reader
	run func()
}

func (reader *onRead) Read(data []byte) (int, error) {
	if reader.run != nil {
		reader.run()
		reader.run = nil
	}
	return reader.Reader.Read(data)
}

func TestCredentialRegistrationWithReadOnlySymlinkedConfig(t *testing.T) {
	directory, options := fileOptions(t)
	static, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(options.Config)
	static = bytes.Replace(static, []byte("managedFile: managed.yaml"), []byte("managedFile: "+filepath.Join(root, "managed.yaml")), 1)
	configDirectory := filepath.Join(root, "readonly")
	if err := os.Mkdir(configDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(configDirectory, "config.yaml")
	if err := os.WriteFile(target, static, 0444); err != nil {
		t.Fatal(err)
	}
	options.Config = filepath.Join(configDirectory, "selected.yaml")
	if err := os.Symlink(target, options.Config); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDirectory, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(configDirectory, 0700) })
	result, err := setFile(t.Context(), directory, options, strings.NewReader("synthetic-secret"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(current, static) {
		t.Fatal("symlink target changed", err)
	}
	info, err := os.Lstat(options.Config)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("static configuration symlink was replaced", err)
	}
	config, err := settings.LoadConfig(options.Config)
	if err != nil || config.Targets[0].Credential.TokenFile != result.RuntimePath {
		t.Fatal("symlinked config did not use managed authentication", err)
	}
}
