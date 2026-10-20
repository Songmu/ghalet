# GitHub App setup wizard

[English](app-setup.md) | [日本語](app-setup.ja.md)

`ghalet auth setup NAME` creates a private, user-owned or organization-owned
GitHub App using [GitHub's Manifest flow][manifest]. No preexisting PAT, client
secret, hosted callback service or PEM copy/paste is required. This wizard
supports **GitHub.com and one configured target per App** on Linux/macOS.
It stores an [ordinary, unencrypted credential file](file-credentials.md);
it does not start or reconfigure the daemon.

## Local setup

Create a configuration with the intended scope, without adding credentials yet:

```yaml
targets:
  - id: project
    scope: OWNER/REPO
```

Use `ORG` instead of `OWNER/REPO` for an organization target. Then run as the
daemon user, with the same state-directory environment:

```sh
ghalet auth setup project-app --target project
```

Select another configuration with `--config PATH`. The default is
`~/.config/ghalet/config.yaml`. `--target` can be omitted if there is exactly
one target. Multiple targets/owners require separate setup invocations and
credential names; the wizard does not broaden an existing App's permissions.

1. Review the target, App owner and requested write permission in the terminal:
   repository `Administration`, or organization `Self-hosted runners`.
2. In the local browser page, choose **Continue to GitHub**. Sign in as the
   intended account, review the suggested App name and create the App.
   Organization policies and your account's permissions may require an owner.
3. GitHub redirects to the local callback. The CLI exchanges the one-time code,
   saves the key and public App metadata, and opens the App installation page.
4. Install on the target account. For a repository target, choose **Only select
   repositories** and select that repository. Wait for any required approval.
5. The CLI discovers the installation, verifies its App, account and required
   write permission, then displays the target-specific configuration changes.
   Answer `y` to save, or decline and resume later. `--yes` skips only this
   final configuration confirmation, not GitHub's approvals.

Other targets and `defaults.credential` are not changed. The selected target
gets `type: github-app`, `clientId`, `privateKeyFile` and `installationId`.
No user OAuth authorization or webhook events are requested; webhook delivery
is disabled. The generated App is private to its owning account.

The temporary loopback server closes after the Manifest exchange or on failure.
It checks the callback's random `state` and Host and does not expose the key in
browser pages, URLs or command output. Installation discovery uses App JWTs,
not a user token or an installation ID supplied by the browser.

After reviewing the saved configuration:

```sh
ghalet config validate
ghalet run
```

For an existing daemon, use [`ghalet apply`](operations.md#applying-configuration)
explicitly instead of starting another one. A successful setup verifies the
installation and its permissions, not live runner/job execution.

## SSH and manual browser operation

The browser must reach the CLI's loopback callback. For an SSH host, forward a
fixed port from the browser's machine:

```sh
ssh -L 8765:127.0.0.1:8765 runner-host
# In that SSH session:
ghalet auth setup project-app --target project \
  --listen 127.0.0.1:8765 --no-browser
```

Open the printed URLs in your local browser while keeping SSH connected.
`--no-browser` alone does not make a remote localhost reachable. The listener
accepts only loopback IP addresses, never `0.0.0.0` or a public interface.
For local manual operation, `--no-browser` works without SSH forwarding.
No public callback or webhook endpoint is needed.

## Resume and credential storage

The key is stored as `<StateDir>/credentials/NAME.pem`; `NAME.app.json` retains
the public App identity, scope and key path so setup can resume. Both files are
`0600` under the `0700` credential directory. The JSON does not contain the PEM,
client secret or webhook secret. Protect the PEM and its backups as described
in [ordinary credential files](file-credentials.md).

Each attempt waits up to 15 minutes. Rerun the same command, configuration,
target and name after interruption, declined confirmation or installation
approval. Once metadata is saved, setup reuses that App/key without creating
another App or rotating the key. It refuses mismatched scopes, owners, key
paths, orphaned existing keys, concurrent setup for the same name, or concurrent
configuration edits. It never automatically deletes a GitHub App.

If App creation completed in GitHub but the callback/exchange failed before
local credentials were saved, inspect the App's settings before starting over.
The Manifest exchange must complete within GitHub's one-hour limit.
If a key was saved but metadata could not be written, the error reports the
retained key path; use the existing App's settings and manual credential import
rather than blindly creating another App.

The wizard writes only configurations owned by the current user and not
writable by others. It preserves comments, although YAML formatting and aliases
may be normalized. Installation permission checks do not change runner group
access policy.

For the packaged systemd service, import the resulting PEM with
[`ghalet systemd credential set`](credentials.md#github-apps-and-additional-credentials).
Then point the selected target's explicit `credential.privateKeyFile` at the
reported systemd runtime path (or remove its explicit credential to inherit the
new shared default), retaining its `installationId`. Merely changing the shared
default does not override the wizard's explicit target credential.
Validate the service configuration before starting it, and remove any plaintext
copy only after verifying encrypted registration and retaining an appropriate
recovery plan. Browser setup and privileged service registration are separate.

App private keys do not expire. Installation-token renewal is automatic;
exchange the key only when required by policy or suspected compromise.
See [authentication and rotation](authentication.md#rotate-credentials).
This feature does not establish isolation from jobs running as the same user.

[manifest]: https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
