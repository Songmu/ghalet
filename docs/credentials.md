# Encrypted credentials for systemd

[日本語](./credentials.ja.md)

For PAT creation, GitHub App registration and minimum GitHub permissions, start
with [authentication setup](./authentication.md). This guide covers storing those
credentials for the packaged Linux service.
For direct Linux/macOS execution without systemd, use
[ordinary credential files](./file-credentials.md).

To create a new App and configure its encrypted key in one flow, use
[`ghalet systemd app setup --scope OWNER/REPO --scale-set project`](./app-setup.md#packaged-systemd-service).
The commands below import existing PATs and App keys.

`ghalet systemd credential set` registers a named PAT or GitHub App private key
for the packaged `ghalet.service`. It requires Linux, root, systemd 250 or later,
`systemd-creds`, and `systemctl`; `--prompt` also requires
`systemd-ask-password` and an interactive terminal. Packages never request
credentials during installation or upgrades, and this command never starts
or restarts the service.

## Register a PAT

First copy and edit the [package configuration](./distribution.md#debian-and-rpm-packages),
including the target scopes. Then register a PAT:

```sh
sudo ghalet systemd credential set default --prompt
```

The default type is `pat`. Select exactly one input method; no input method
is chosen implicitly:

```sh
sudo ghalet systemd credential set default --file /secure/pat-file
sudo ghalet systemd credential set default --stdin < /secure/pat-file
```

`--file` requires an owner-only regular file, normally mode `0600`, and does
not delete or modify it. Do not put secrets in command arguments, shell
history, YAML values, or environment variables. Prompt cancellation, empty
input, invalid keys and encryption failures leave existing registrations
unchanged. Input is limited to 1 MiB.

Registration encrypts directly from memory with `systemd-creds`, using
`--name=default --with-key=host`. Only ciphertext is written to
`/etc/ghalet/credentials/default.cred` with mode `0600`. The host encryption
key is `/var/lib/systemd/credential.secret`: protect and plan backups of it
along with the encrypted credentials. Encryption does not protect against
root with access to that key, and another host cannot decrypt the ciphertext
without the original key.

The command writes its dedicated drop-in and runs `systemctl daemon-reload`:

```ini
# /etc/systemd/system/ghalet.service.d/credential-default.conf
[Service]
LoadCredentialEncrypted=default:/etc/ghalet/credentials/default.cred
```

It also saves `defaults.credential` in `/var/lib/ghalet/managed.yaml`, without
rewriting `/etc/ghalet/config.yaml`. Static defaults and explicit target
authentication remain authoritative. Select a different configuration with
`--config PATH`; this does not change the unit's `ExecStart`.
See [managed setup information](./operations.md#managed-setup-information) for
path overrides, custom systemd configurations and precedence.

```yaml
# /var/lib/ghalet/managed.yaml
defaults:
  credential:
    type: pat
    tokenFile: /run/credentials/ghalet.service/default
```

Only targets that omit `credential` inherit the shared default;
`credential: {}` selects `gh`. See [credential inheritance](./operations.md#configuration-and-credentials)
for override rules.

Validate and start explicitly:

```sh
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
sudo journalctl -u ghalet -f
```

Validation checks configuration, not GitHub authentication or permissions.
Systemd supplies the decrypted credential in a private, read-only runtime
directory at service startup. Standalone processes outside the service do
not automatically receive those runtime files.
ghalet accepts root-owned files under `/run/credentials/UNIT/` with a read-only
ACL for the service's effective UID, provided the parent directories are
root-owned and not writable by other users and no group or unrelated user has
effective access. Ordinary credential files still require owner-only permissions.
Do not `chmod` systemd's runtime files: changing the ACL mask can remove the
service user's access.

## GitHub Apps and additional credentials

Import one unencrypted RSA PEM private key (PKCS#1 or PKCS#8):

```sh
sudo ghalet systemd credential set app-work-key --type github-app \
  --client-id APP_CLIENT_ID --installation-id 12345678 --file /secure/app.pem
```

This replaces the managed shared default with `type: github-app`, `clientId`,
`installationId`, and `privateKeyFile: /run/credentials/ghalet.service/app-work-key`.
An existing App default's client ID is reused if `--client-id` is omitted.
If `--installation-id` is omitted, the existing default's installation ID is
reused only when its client ID matches. Otherwise an installation ID is required.
Installation tokens are acquired by the existing SDK, not entered manually.

Use `--no-default` to register another secret without modifying managed defaults:

```sh
sudo ghalet systemd credential set secondary --prompt --no-default
```

Set the selected target's `credential.type: pat` and
`credential.tokenFile: /run/credentials/ghalet.service/secondary`.
App keys use `type: github-app`, `clientId`, `privateKeyFile`, and
`installationId` together inside `credential`. Different targets can share a path or
reference different names. Names contain 1-64 ASCII letters, digits,
underscores or hyphens, starting with a letter or digit.

## Rotation and rollback

Create the new PAT before revoking the old one, or generate an additional
App key before deleting the old key. For rotation, keep the credential name,
type, and App identity unchanged. Register the new value using `--no-default`
so configuration stays unchanged:

```sh
sudo ghalet systemd credential set default --prompt --no-default
sudo systemctl stop ghalet
# Check the daemon's exit result and logs; continue only after successful shutdown.
sudo systemctl start ghalet
```

App keys use the same command with `--type github-app --file /secure/new.pem`
instead of `--prompt`. The ciphertext is replaced atomically, and the old
ciphertext is retained at `/etc/ghalet/credentials/NAME.cred.previous`.
Another update is refused until that backup is resolved. Unexpected changes
to the command's drop-in are not overwritten. Failures during file updates
or daemon reload attempt to restore the prior files and report any rollback
errors explicitly. If file restoration fails, the previous ciphertext backup
is retained and its path is included in the error.

Unlike ordinary credential files reread by `apply`, encrypted systemd
credentials refresh only on service start. Stop normally and verify successful
shutdown as shown above; resolve [forced or incomplete drain](./operations.md#capacity-retries-and-shutdown)
before starting again. All targets sharing the credential switch together.
After verifying authentication and runner operation for every affected target,
revoke the old PAT or remove the old App key, then remove its rollback backup:

```sh
sudo rm -- /etc/ghalet/credentials/default.cred.previous
```

If the new credential fails, keep the old credential valid. Stop normally,
verify successful shutdown, restore its encrypted backup, then start again:

```sh
sudo systemctl stop ghalet
# Check the daemon's exit result and logs; continue only after successful shutdown.
sudo mv -- /etc/ghalet/credentials/default.cred.previous /etc/ghalet/credentials/default.cred
sudo systemctl start ghalet
```

Rollback of a rotation does not change YAML. When changing authentication
type or App identity rather than rotating it, separately retain and restore
the corresponding configuration. PAT expiration, organization approval and
GitHub App private-key rotation still require operator management.
