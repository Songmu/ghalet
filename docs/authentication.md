# Authentication setup and minimum permissions

[English](authentication.md) | [日本語](authentication.ja.md)

This guide covers issuing credentials for ghalet's repository and organization
targets on GitHub.com. Prefer a GitHub App for a long-lived service, or a
fine-grained personal access token (PAT) for a smaller setup. `type: gh` reuses
GitHub CLI's token; it does not grant additional permissions.
For other GitHub hosts, check that host's API documentation and policies.

## Choose an authentication method

| Method | Advantages | Tradeoffs |
| --- | --- | --- |
| GitHub CLI (`gh`, default) | Reuses an existing login without a separate ghalet secret file; convenient for local trials | Shares the selected token's authority and lifecycle. Requires `gh` and access to a token through its environment or stored login/keychain; an OAuth login is not repository-scoped |
| Fine-grained PAT (`pat`) | Select individual repositories and permissions; simple file-based setup | Tied to a person and one resource owner. Expiration, replacement and any organization approval need operator management |
| GitHub App (`github-app`) | Installation identity rather than a person's login; selected repositories and permissions; SDK renews short-lived installation tokens | More initial setup, installation approval and ID/key management. The private key does not expire; protect it and replace it if compromised or required by policy |

Use a dedicated App for shared or long-lived deployments, a fine-grained PAT
for a simple dedicated setup, and `gh` for an existing interactive environment.
See GitHub's [PAT guidance][pat-guide], [App overview][app-overview] and
[installation authentication guide][app-installation].
None of these methods isolates jobs from credentials accessible to their OS
user; retain the [trusted-host boundary](operations.md#trust-boundary).

## Minimum permissions

Choose permissions for the target's `scope`, not for where workflows are stored.
A repository target and an organization target have different requirements:

| Credential | Repository scope (`OWNER/REPO`) | Organization scope (`ORG`) |
| --- | --- | --- |
| Fine-grained PAT | Repository **Administration: Read and write**, limited to the target repositories | Organization **Self-hosted runners: Read and write**, with the target organization as resource owner |
| GitHub App | Repository **Administration: Read and write**, installed on the target repositories | Organization **Self-hosted runners: Read and write**, installed on the target organization |
| GitHub CLI OAuth token | `repo` | `admin:org`; the REST API documentation also requires `repo` for private repositories |

Keep the automatically included repository **Metadata: Read-only** permission.
For an organization-only App, repository Administration is not required.
For mixed repository and organization targets, grant both required permissions,
or use separate credentials to keep their authority separate.

These minima follow the [repository][repo-registration] and
[organization][org-registration] registration-token endpoints used by the
[pinned Scale Set SDK][sdk-auth]. The SDK exchanges that token for Actions
service credentials to manage scale sets, runner groups and runners.
ghalet does not need Contents, Workflows or Actions permissions on the
management credential. A job's `GITHUB_TOKEN` and workflow `permissions` are
separate; granting them more permissions does not authorize ghalet.
Short-lived runner registration tokens are not a supported credential input.

A PAT cannot exceed its owner's access: use a repository administrator for
repository targets or an organization owner for organization targets.
Organization policies can restrict or require approval for tokens and Apps.
Repository Administration and `repo` are broad privileges, not runner-only
access; fine-grained tokens and App installations limit the affected repositories.
Organization runner permissions are organization-wide, not restricted by
selecting repositories. Independently configure the runner group's repository
access in GitHub; ghalet references existing groups and does not change that policy.

## Create a fine-grained PAT

Follow [GitHub's PAT creation guide][pat-guide]:

1. Open your personal **Settings → Developer settings → Personal access tokens
   → Fine-grained tokens → Generate new token**.
2. Set a descriptive name and an expiration date. Choose the repository owner
   or target organization as **Resource owner**. One fine-grained PAT covers
   only one resource owner; use separate tokens for different owners.
3. For repository targets, choose **Only select repositories**, select only the
   targets, and grant repository **Administration: Read and write**.
   For organization targets, grant organization **Self-hosted runners:
   Read and write**; do not grant repository permissions just to run
   organization-level runners.
4. Leave other permissions unset, then generate the token. If organization
   approval is required, wait for approval before using it.
5. Store the value securely when it is displayed. For the packaged Linux
   service, use the [encrypted PAT registration procedure](credentials.md#register-a-pat).

For direct execution on Linux or macOS, put the PAT in an owner-only regular
file readable by the daemon user, then reference it without copying the value
into YAML:

```sh
chmod 600 /absolute/path/to/ghalet.pat
```

```yaml
defaults:
  credential:
    type: pat
    tokenFile: /absolute/path/to/ghalet.pat

targets:
  - id: project
    scope: OWNER/REPO
```

Classic PATs are also supported with `type: pat`, but use a fine-grained PAT
or GitHub App for new setups.

`tokenEnv` is an alternative to `tokenFile`, not an additional field;
avoid environment-based secret storage for the packaged service. Never put
token values in command arguments, shell history, committed files or runner
environment settings.

## GitHub CLI

For `type: gh`, log in as the daemon's execution user. If the existing login
uses a GitHub CLI OAuth token, request the required scope:

```sh
# Repository targets.
gh auth refresh --hostname github.com --scopes repo
# Organization targets; include repo as well when required for private repositories.
gh auth refresh --hostname github.com --scopes admin:org
```

These commands add scopes to the CLI login, which may already have other scopes;
they do not create a dedicated least-privilege token. If GitHub CLI is supplied a
PAT instead, change that PAT's permissions or replace it; refreshing OAuth scopes
does not upgrade a PAT. See the [GitHub CLI scope refresh reference][gh-refresh]
and [service-user authentication](operations.md#configuration-and-credentials)
for keychain and service-environment considerations.

## Create and install a GitHub App

Follow [GitHub's App registration guide][app-guide]:

1. Open the owning account's **Settings → Developer settings → GitHub Apps
   → New GitHub App**. For organization runners, use an organization-owned App.
2. Give it a unique name and a **Homepage URL**, such as this project's
   repository URL. Leave callback and setup URLs blank, and do not enable
   user authorization or device flow: ghalet uses installation authentication.
3. Under **Webhook**, deselect **Active**. No webhook URL, public listener or
   event subscriptions are needed.
4. Set only the permissions for the intended scopes from the table above.
   Leave other permissions at **No access**, apart from automatic Metadata read
   access. For an App used only by its owning account, choose **Only on this
   account** under installation availability, then create the App.
5. On the App's settings page, copy the **Client ID** for ghalet's `clientId`.
   This is not the client secret or the installation ID; no client secret is
   needed. Under **Credentials → Key pairs**, select **New key** (or
   **Private keys → Generate a private key** in the older settings layout),
   and securely save the downloaded PEM file.
6. Select **Install App** and install it on the account that owns the target.
   For repository targets, choose **Only select repositories** and select just
   those repositories. Organization targets require installation on that
   organization; selecting repositories does not restrict organization-level
   runner authority.
7. Open the installation's configuration page and copy the numeric
   **installation ID** from the final URL segment, for example
   `https://github.com/organizations/ORG/settings/installations/12345678`
   or `https://github.com/settings/installations/12345678`.
   Set `installationId: 12345678` on each target using that installation.

For direct execution, make the downloaded private key readable only by its
owner (`chmod 600 /absolute/path/to/ghalet-app.pem`) and configure:

```yaml
defaults:
  credential:
    type: github-app
    clientId: Iv1.REPLACE_WITH_CLIENT_ID
    privateKeyFile: /absolute/path/to/ghalet-app.pem

targets:
  - id: project
    scope: OWNER/REPO
    installationId: 12345678
```

For an organization target, change `scope` to `ORG` and use that organization's
installation ID. Multiple installations can share the App's client ID and key,
but each target must specify the installation ID for its owner.
ghalet's SDK obtains and renews installation tokens automatically.
For the packaged service, follow [App key import](credentials.md#github-apps-and-additional-credentials)
instead of storing the key directly in the configuration directory.
Protect the key: it can authenticate as any installation of that App.
See [GitHub's private-key guidance][app-keys] and the
[credential rotation procedure](credentials.md#rotation-and-rollback).

## Rotate credentials

GitHub App private keys [do not expire][app-keys], so periodic manual replacement
is not required to keep authentication working. Replace a key when required by
your security policy or if compromise is suspected. The SDK automatically
obtains new short-lived installation tokens; this is separate from replacing
the private key.

PATs still need replacement before expiration: the SDK's renewal of Actions
service credentials does not extend a PAT's expiration or replace a revoked token.

When replacing the packaged service's encrypted credentials, use
[systemd rotation and rollback](credentials.md#rotation-and-rollback):
register the replacement under the same name with `--no-default`, then stop
normally, confirm successful shutdown, and start the service. `apply` cannot
refresh systemd's decrypted credential files.

For planned replacement of ordinary PAT files or App key files on Linux/macOS:

1. Create a **new** PAT with the same minimum permissions and repository
   selection, or generate an **additional** private key on the existing App.
   Complete any approval/SSO steps first. Keep the old credential valid during
   this planned transition; do not regenerate or revoke it first.
2. Retain a protected copy of the old file for rollback, then atomically replace
   the configured file with the new value. Preserve owner-only permissions and
   readability by the daemon user. An App key rotation keeps the same
   `clientId` and target `installationId` values.
3. Run `ghalet apply --config /absolute/path/to/config.yaml`. It drains all
   targets and creates new clients that reread the files, even if their paths
   have not changed. Plan for the [drain deadline](operations.md#capacity-retries-and-shutdown);
   rotation is not guaranteed to be interruption-free.
4. Check the apply result, `ghalet doctor`, daemon logs and a trusted test job
   for **every target sharing that credential**. Only after success, revoke
   the old PAT or delete the old App key in GitHub and remove protected rollback
   copies that are no longer needed.

If the replacement fails, restore the old protected file and run `apply` again
before revoking anything. When changing authentication type or App identity,
retain and restore the matching YAML as well. Apply failures are not automatic
configuration rollbacks.

For `tokenEnv`, changing your shell's variable does not change the running
daemon's environment: update its launch environment and perform a normal
stop/start, confirming successful shutdown first.

For `type: gh`, `GH_TOKEN` and `GITHUB_TOKEN` in the daemon's environment
(in that order) [override the stored login][gh-environment]. If either supplies
the token, update the daemon's launch environment and perform the same normal
stop/start; changing the stored login or running `apply` alone does not replace
that environment token. Only when no environment token overrides the stored
login, replace or reauthorize the login as the daemon user and run `apply`
so new clients obtain the current token.
See [configuration apply](operations.md#applying-configuration).
If a credential is exposed, revoke it immediately rather than using the routine
overlap procedure; restore service with a clean replacement.

## Check the setup

Run `ghalet config validate`, then `ghalet run` and `ghalet doctor` with the
same configuration and execution user. Configuration validation is local only:
it does not prove GitHub permissions. Confirm that the daemon registers the
target scale set and executes a trusted test workflow using its `runs-on` name.

If authentication or registration fails, check token expiration/revocation,
PAT owner access, resource owner and approval, App permissions and installation
approval, selected repositories, and the client ID / installation ID / key
combination. Runner group access must also allow the workflow's repository.
Do not fix an access error by granting every permission.

[repo-registration]: https://docs.github.com/en/rest/actions/self-hosted-runners#create-a-registration-token-for-a-repository
[org-registration]: https://docs.github.com/en/rest/actions/self-hosted-runners#create-a-registration-token-for-an-organization
[sdk-auth]: https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/client.go
[pat-guide]: https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens
[app-guide]: https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app
[app-keys]: https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps
[app-overview]: https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/about-creating-github-apps
[app-installation]: https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation
[gh-refresh]: https://cli.github.com/manual/gh_auth_refresh
[gh-environment]: https://cli.github.com/manual/gh_help_environment
