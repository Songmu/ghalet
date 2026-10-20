package cmd

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/Songmu/ghalet/credentialstore"
)

func TestCredentialOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"default", "--prompt"}, ""},
		{[]string{"default", "--stdin"}, ""},
		{[]string{"--stdin", "default"}, ""},
		{[]string{"app", "--type", "github-app", "--file", "key.pem", "--client-id", "app-client"}, ""},
		{[]string{"default"}, "select exactly one"},
		{[]string{"default", "--prompt", "--stdin"}, "select exactly one"},
		{[]string{"default", "--file", "token", "--stdin"}, "select exactly one"},
		{[]string{"../escape", "--stdin"}, "credential name"},
		{[]string{"bad.name", "--stdin"}, "credential name"},
		{[]string{"bad\nname", "--stdin"}, "credential name"},
		{[]string{strings.Repeat("x", 65), "--stdin"}, "credential name"},
		{[]string{"default", "--type", "gh", "--stdin"}, "--type"},
		{[]string{"app", "--type", "github-app", "--prompt"}, "GitHub App keys require"},
		{[]string{"default", "--client-id", "client", "--stdin"}, "--client-id"},
		{[]string{"default", "--stdin", "secret-must-not-appear"}, "exactly one name"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			options, err := parseCredentialOptions(tc.args, "/selected/config.yaml", &stderr)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if options.Config != "/selected/config.yaml" {
					t.Fatal("global config flag was lost")
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret-must-not-appear") {
				t.Fatal("argument contents were disclosed")
			}
		})
	}
}

func TestCredentialHelp(t *testing.T) {
	for _, args := range [][]string{
		{"systemd", "--help"},
		{"systemd", "-h"},
		{"systemd", "credential", "--help"},
		{"systemd", "credential", "-h"},
		{"systemd", "credential", "set", "--help"},
		{"systemd", "credential", "set", "default", "--help"},
		{"systemd", "credential", "set", "--config", "custom.yaml", "--help"},
		{"--config", "custom.yaml", "systemd", "--help"},
		{"--config", "custom.yaml", "systemd", "credential", "set", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(t.Context(), args, &stdout, &stderr); !errors.Is(err, flag.ErrHelp) {
			t.Fatal(args, err)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: ghalet systemd credential set") {
			t.Fatal("help required privileges or wrote to stdout", stdout.String(), stderr.String())
		}
	}
}

func TestSystemdCredentialCommandRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"systemd"}, "systemd requires the credential subcommand"},
		{[]string{"systemd", "unknown"}, "systemd requires the credential subcommand"},
		{[]string{"systemd", "credential"}, "systemd credential requires the set subcommand"},
		{[]string{"systemd", "credential", "unknown"}, "systemd credential requires the set subcommand"},
		{[]string{"systemd", "credential", "set", "default"}, "select exactly one"},
		{[]string{"--config", "custom.yaml", "systemd", "credential", "set", "default"}, "select exactly one"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := Run(t.Context(), tc.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatal("invalid command reported success", stdout.String())
			}
		})
	}
}

func TestCredentialCommandRequiresSystemdNamespace(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(t.Context(), []string{"credential", "set", "default", "--stdin"}, &stdout, &stderr)
	if err == nil || stdout.Len() != 0 {
		t.Fatal("accepted credential without the systemd namespace", err, stdout.String())
	}
}

func TestPrintCredentialResult(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		noDefault bool
		rotation  bool
	}{
		{name: "pat", kind: "pat"},
		{name: "rotation", kind: "pat", noDefault: true, rotation: true},
		{name: "app", kind: "github-app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := credentialstore.Options{
				Name: "default", Type: tc.kind, Config: "/etc/ghalet/config.yaml", NoDefault: tc.noDefault,
			}
			result := credentialstore.Result{
				EncryptedPath: "/etc/ghalet/credentials/default.cred",
				DropInPath:    "/etc/systemd/system/ghalet.service.d/credential-default.conf",
				RuntimePath:   "/run/credentials/ghalet.service/default",
			}
			field := "tokenFile"
			if tc.kind == "github-app" {
				field = "privateKeyFile"
			}
			want := "Registered credential default.\n" +
				"Encrypted file: /etc/ghalet/credentials/default.cred\n" +
				"Systemd drop-in: /etc/systemd/system/ghalet.service.d/credential-default.conf\n" +
				field + ": /run/credentials/ghalet.service/default\n"
			if !tc.noDefault {
				want += "Updated defaults.credential in /etc/ghalet/config.yaml.\n"
			}
			if tc.rotation {
				result.PreviousPath = result.EncryptedPath + ".previous"
				want += "Previous encrypted credential: /etc/ghalet/credentials/default.cred.previous\n" +
					"Stop ghalet normally and verify successful shutdown from its exit result and logs, then start it and verify all affected targets before revoking the old credential and deleting this backup.\n"
			} else {
				want += "The service was not started or restarted. Validate the configuration before starting it.\n"
			}
			var out bytes.Buffer
			if err := printCredentialResult(&out, options, result); err != nil {
				t.Fatal(err)
			}
			if out.String() != want {
				t.Fatalf("got %q, want %q", out.String(), want)
			}
		})
	}
}

func TestPrintCredentialResultWriteError(t *testing.T) {
	err := printCredentialResult(failingWriter{}, credentialstore.Options{}, credentialstore.Result{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("output error was not returned", err)
	}
}
