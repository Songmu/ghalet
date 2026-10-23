# 導入ガイド

[English](./getting-started.md)

専用の Linux / macOS ホストへのインストールから、最初の GitHub Actions ジョブを実行するまでの手順です。runner はジョブの厳密な隔離を保証しないため、信頼できる workflow のみに使用してください。

[信頼境界](./operations.ja.md#信頼境界)も参照してください。

## インストール

Homebrew でインストールできます。

```sh
brew install Songmu/tap/ghalet
```

または Go でインストールできます。

```sh
go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

systemd unit を含む Linux 向け `.deb` / `.rpm` パッケージと、GHCR のコンテナイメージも利用できます。導入・設定・更新の手順は[パッケージとコンテナ](./distribution.ja.md)を参照してください。

インストール先を `PATH` に追加してください。workflow で使うビルドツールと、公式 runner が要求する OS の依存パッケージをホストに用意してください。

通常は ghalet を非 root ユーザーで起動します。デーモンを root で起動する場合は、`runner.user` に既存の非 root ユーザーを指定する必要があります。root での runner ジョブ実行は拒否されます。実行ユーザーと環境の設定は[運用ガイド](./operations.ja.md#直接実行と環境)を参照してください。

## クイックスタート

以下は、1つのリポジトリで runner を動かす手順です。ghalet を起動する非 root ユーザーで実行してください。

継続運用では、Linux は systemd、macOS は launchd（Homebrew では `brew services`）でサービスとして常駐させる構成を基本とします。以下ではまずフォアグラウンドで動作を確認し、その後に常駐化します。

常駐化の手順は以下を参照してください。

- Linux: [パッケージのサービス設定](./distribution.ja.md#debian--rpm-パッケージ)。
- macOS: [Homebrew での常駐化](#homebrew-での常駐化macos-のみ)。

### 1. GitHub に認証する

GitHub CLI（`gh`）を用意し、対象リポジトリの runner を管理できるアカウントでログインします。必要な権限は[認証のセットアップ](./authentication.ja.md)を参照してください。

```sh
gh auth login
```

この手順では `gh` のログインを使います。PAT や GitHub App を使う場合も、[認証のセットアップ](./authentication.ja.md)を参照してください。

### 2. runner の設定を作成する

`~/.config/ghalet/config.yaml` を作成します。`OWNER/REPO` は自分のリポジトリに置き換えてください。Organization 全体で使う場合は、代わりに `ORG` を指定します。

```sh
mkdir -p ~/.config/ghalet
cat > ~/.config/ghalet/config.yaml <<'YAML'
targets:
  - scope: OWNER/REPO
    scaleSet: ghalet-runner
YAML
ghalet config validate
```

`scope` は runner を登録するリポジトリまたは Organization、`scaleSet` は workflow の `runs-on` に指定する任意の名前です。この例では `ghalet-runner` に揃えます。リポジトリ名と同じにする必要はありません。別の設定ファイルを使う場合は、各コマンドに `--config /path/to/config.yaml` を指定してください。その他の設定は[コメント付きのサンプル](../examples/config.yaml)を参照してください。

### 3. workflow から runner を指定する

対象リポジトリに `.github/workflows/runner-check.yaml` を作成します。`runs-on` を先ほどの `scaleSet` と同じ `ghalet-runner` にします。信頼できる workflow のみに使用してください。

```yaml
name: Runner check
on: push

jobs:
  check:
    runs-on: ghalet-runner
    steps:
      - run: uname -a
```

### 4. ghalet を起動してジョブを実行する

ホストでデーモンを起動します。ジョブが届くと runner が自動で起動します。

```sh
ghalet run
```

workflow をコミットして対象リポジトリに push し、GitHub の Actions タブで `Runner check` が実行されることを確認してください。別のターミナルから、ホスト上の状態確認と診断も実行できます。

```sh
ghalet status
ghalet doctor
```

デーモンを停止するには、起動したターミナルで Ctrl-C を押してください。通常停止では全 target の新規ジョブ受付を止め、設定した drain の期限まで受付済みの処理を待ちます。期限を超えたジョブは強制停止される場合があります。設定を編集した後は `ghalet apply`（パッケージのサービスでは `sudo systemctl reload ghalet`）で、デーモンを終了せずに drain と再設定を行えます。

## Homebrew での常駐化（macOS のみ）

Homebrew でインストールした場合、設定と認証を済ませた後、`ghalet run` の代わりに `brew services` でバックグラウンド実行できます。フォアグラウンドで起動済みの場合は、先に Ctrl-C で停止してください。設定と認証を用意したユーザーで、`sudo` を付けずに実行します。デフォルトの設定ファイル `~/.config/ghalet/config.yaml` を使用します。

```sh
# 起動し、ログイン時の自動起動を有効化
brew services start ghalet

# 停止し、ログイン時の自動起動を無効化
brew services stop ghalet
```

ログイン前には起動せず、ホストのスリープも防ぎません。
