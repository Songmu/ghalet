# Ordinary credential files

[日本語](file-credentials.ja.md)

`ghalet credential set` imports a named PAT or GitHub App private key on Linux
or macOS without systemd or root. It writes **unencrypted, owner-only files**,
not a keychain or encrypted vault. For GitHub permissions and credential creation,
see [authentication setup](authentication.md). For the packaged service's
encrypted storage, use [systemd credentials](credentials.md) instead.

## Register a credential

Create the [configuration](getting-started.md#quick-start) first, then run as the
user that runs the daemon:

```sh
ghalet credential set default --prompt
ghalet credential set default --file /secure/pat-file
ghalet credential set default --stdin < /secure/pat-file
```

Select exactly one input method. The default type is `pat`; `--prompt` uses an
interactive terminal without echo, with a two-minute timeout. `--file` requires
an owner-only regular file and does not modify it. Input is limited to 1 MiB;
secrets must not appear in arguments, shell history, YAML or runner settings.

For an App, import an unencrypted RSA PEM key (PKCS#1 or PKCS#8):

```sh
ghalet credential set app-work --type github-app \
  --client-id APP_CLIENT_ID --file /secure/app.pem
```

Files are placed in `<StateDir>/credentials/NAME.pat` or `NAME.pem`, with mode
`0600`, under a directory with mode `0700`. The state directory follows
`STATE_DIRECTORY`, otherwise `$XDG_STATE_HOME/ghalet` or
`~/.local/state/ghalet`. Run registration with the same environment as the
daemon. Credential files are separate from `runs/` and `logs/` and are not
removed by `ghalet gc`.

The command writes `defaults.credential` to the selected configuration's
[managed file](operations.md#managed-setup-information) with
`type: pat` / `tokenFile`, or `type: github-app` / `clientId` / `privateKeyFile`.
The paths are absolute. The default configuration is
`~/.config/ghalet/config.yaml`; override it with `--config PATH`.
An existing App default's client ID is reused if `--client-id` is omitted.
Each App target still needs its own `installationId`.
The static configuration is never changed. Explicit static defaults and target
credentials remain authoritative.

Use `--no-default` to save a credential without modifying managed defaults,
then reference its reported path in the desired target. Names follow the same
rules as [systemd credential names](credentials.md#github-apps-and-additional-credentials).
Existing unsafe permissions or symlinks are rejected rather than silently fixed.

The daemon is never started, restarted or reconfigured automatically.
Run `ghalet config validate`, then start it or use
[`ghalet apply`](operations.md#applying-configuration) explicitly.

## Replacement and rollback

Create a new PAT before the old one expires. App keys do not expire; replace
them when required by policy or suspected compromise, rather than for token
renewal. See [credential rotation](authentication.md#rotate-credentials).

For a planned replacement, keep the name, type and App identity unchanged:

```sh
ghalet credential set default --prompt --no-default
ghalet apply --config /absolute/path/to/config.yaml
```

The old value is retained in `NAME.pat.previous` or `NAME.pem.previous`,
also mode `0600`. Another replacement is refused until this backup is resolved.
Verify every affected target before revoking the old credential and deleting
the backup. If replacement fails, restore the backup to the reported credential
path and apply again while the old credential is still valid.
If registration fails during file updates, it attempts to restore prior files
and explicitly reports restoration errors.

Owner-only permissions do not protect against root or jobs running as the same
OS user. Keep the [trusted-host boundary](operations.md#trust-boundary); use a
separate execution user where appropriate. Backups also contain plaintext secrets.
