# ghalet

[![Test Status](https://github.com/Songmu/ghalet/actions/workflows/test.yaml/badge.svg?branch=main)][actions]
[![Coverage Status](https://codecov.io/gh/Songmu/ghalet/branch/main/graph/badge.svg)][codecov]
[![MIT License](https://img.shields.io/github/license/Songmu/ghalet)][license]
[![PkgGoDev](https://pkg.go.dev/badge/github.com/Songmu/ghalet)][PkgGoDev]

[actions]: https://github.com/Songmu/ghalet/actions?workflow=test
[codecov]: https://codecov.io/gh/Songmu/ghalet
[license]: https://github.com/Songmu/ghalet/blob/main/LICENSE
[PkgGoDev]: https://pkg.go.dev/github.com/Songmu/ghalet

[English](README.md) | [日本語](README.ja.md)

ghalet は、Linux / macOS ホストを、ジョブ需要に応じて自動で増減する
GitHub Actions のセルフホステッド runner 環境にするツールです。余っている Mac、
専用の Linux マシン、常時起動する VM を、信頼できるリポジトリのために活用できます。
Kubernetes や Docker、ジョブごとの VM を用意する必要はありません。

Go 製のデーモンが GitHub Actions Runner Scale Set を管理し、ジョブが届くと
ネイティブの runner プロセスを起動します。各 runner は 1 ジョブを実行して終了し、
ghalet が runner 専用ディレクトリと workspace を削除します。複数のリポジトリや
Organization で 1 台のホストを共有し、target ごと・ホスト全体の並列実行数を制限できます。

## 主なユースケース

- **余っている Mac を macOS ビルドに使う。** マシンにインストール済みのツールを使い、
  macOS 上で直接ジョブを実行できます。
- **信頼できる複数のプロジェクトで Linux ホストを共有する。** リポジトリや
  Organization の target を登録し、共通の同時実行上限の範囲で必要な runner を起動します。
- **小さく、作り直しやすい runner 環境を持つ。** 専用アカウントや常設 VM 上で
  デーモンを 1 つ動かす構成です。macOS 上の Lima VM も利用できます。
  設定ファイルを構成の正とし、小さなローカルステートファイルで復旧するため、
  別途データベースは不要です。

ghalet が増減するのは **単一ホスト上の runner プロセス** であり、ホスト自体ではありません。
複数マシンのオーケストレーションや、ジョブごとの VM / コンテナの作成は行いません。
**Ephemeral な登録はジョブの隔離を意味しません。** ジョブは実行ユーザーの HOME、
ツール、キャッシュ、権限を共有します。信頼できる workflow のみに使用し、
信頼できない fork PR や悪意あるコードは実行しないでください。Docker actions、
ジョブコンテナ、サービスコンテナには対応していません。

## インストール

systemd unit を含む Linux 向け `.deb` / `.rpm` パッケージと、
GHCR のコンテナイメージもリリース時にビルドします。
導入・設定・更新の手順は[パッケージとコンテナ](docs/distribution.ja.md)を参照してください。

```console
# 最新版をインストール。デフォルトの配置先は ./bin/。
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s

# 配置先ディレクトリとバージョンを指定。
% curl -sfL https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s -- -b $(go env GOPATH)/bin [vX.Y.Z]

# curl がない Alpine Linux では wget を使用。
% wget -O - -q https://raw.githubusercontent.com/Songmu/ghalet/main/install.sh | sh -s [vX.Y.Z]

# または Go でインストール。
% go install github.com/Songmu/ghalet/cmd/ghalet@latest
```

インストール先を `PATH` に追加してください。workflow で使うビルドツールと、
公式 runner が要求する OS の依存パッケージを
ホストに用意してください。ghalet は非 root ユーザーで起動します。root でのジョブ実行は
拒否されます。実行ユーザーと環境の設定は[運用ガイド](docs/operations.ja.md#直接実行と環境)
を参照してください。

## クイックスタート

設定ファイルを作成し、ghalet を起動するユーザーで GitHub CLI にログインします。
以下の `OWNER/REPO` を自分のリポジトリに置き換えてください。
Organization の場合は `ORG` を指定します。

```sh
mkdir -p ~/.config/ghalet
cat > ~/.config/ghalet/config.yaml <<'YAML'
targets:
  - id: project
    scope: OWNER/REPO
YAML
gh auth login
ghalet config validate
ghalet run
```

設定ファイルのデフォルトは `~/.config/ghalet/config.yaml` です。
別のファイルを使う場合は `--config /path/to/config.yaml` を指定してください。
デフォルトの認証には `gh` のログインを使います。[PAT と GitHub App による認証](docs/operations.ja.md#設定と認証情報)
にも対応しています。その他の設定は[コメント付きのサンプル](examples/config.yaml)を参照してください。

信頼できる workflow の `runs-on` に、設定した Scale Set 名
（`scaleSet`、省略時は `id`）を指定します。

```yaml
jobs:
  build:
    runs-on: project
    steps:
      - run: uname -a
```

別のターミナルから状態確認を実行できます。

```sh
ghalet status
ghalet doctor
```

デーモンを停止するには、起動したターミナルで Ctrl-C を押してください。
通常停止では全 target の新規ジョブ受付を止め、設定した drain の期限まで
受付済みの処理を待ちます。期限を超えたジョブは強制停止される場合があります。
設定を編集した後は `ghalet apply`（パッケージのサービスでは
`sudo systemctl reload ghalet`）で、デーモンを終了せずに drain と再設定を行えます。

## コマンド

すべてのコマンドで `--config PATH` を指定できます。**target** は設定内の
リポジトリまたは Organization の登録単位で、ローカルでは `id` で識別します。

| コマンド | 用途 |
| --- | --- |
| `ghalet run` | フォアグラウンドでデーモンを起動し、設定された全 target のジョブ受付と runner プロセスを管理します。 |
| `ghalet apply` | 設定を再読込し、全 target を drain して登録を反映し、受付を再開します。設定から削除・置換された target の未参照 Scale Set は削除を試みます。 |
| `ghalet config validate` | runner の登録や GitHub への接続を行わず、設定の構文・値・ローカルパスの制約を検証します。 |
| `ghalet systemd credential set NAME --prompt` | Linux の systemd サービス用 PAT を暗号化して登録し、`defaults.credential` を更新します。root 権限が必要です。App キー、自動構築、ローテーションは[認証情報](docs/credentials.ja.md)を参照してください。 |
| `ghalet list` | アクセス可能で応答するデーモンの管理 UID、制御ソケット、稼働状態を一覧表示します。`--config PATH` で絞り込めます。 |
| `ghalet status` | 稼働中デーモンの状態と容量のスナップショットを取得します。取得成功は正常稼働を意味しません。 |
| `ghalet doctor` | 稼働中デーモンの問題を診断し、問題があれば非ゼロで終了します。 |
| `ghalet gc [--dry-run]` | 保留中の実行を再検証し、安全に削除できるものの cleanup と保存ログの期限切れ削除を行います。`--dry-run` では変更せず確認できます。 |

`ghalet --version` でバージョンを表示します。認証情報のセットアップ案内を除き、
管理コマンドの応答は JSON です。`systemd credential set` のデフォルト設定パスは
`/etc/ghalet/config.yaml` です。`status`、`doctor`、`apply`、`gc` は `--config` を
省略すると、アクセス可能なデーモンが 1 件なら自動選択し、複数なら設定の明示を求めます。
見つからない場合は、`run` や `config validate` と同じくユーザーごとの
`~/.config/ghalet/config.yaml` を使います。`apply` は新しい設定やデフォルト設定ではなく、
選択したデーモン自身の設定ファイルを再読込します。
終了コード、オフラインでの保守、削除時の安全確認については
[設定反映](docs/operations.ja.md#設定反映)と
[診断と cleanup](docs/operations.ja.md#診断ガベージコレクション)を参照してください。

## 実行時の動作

ホスト全体の同時実行上限はデフォルトで 2 です。target ごとに待機 runner の最小数と
上限を設定でき、実際に割り当てられたジョブは待機用の warm pool より優先されます。
通常停止では受付済みの処理を待ちます。クラッシュ後は小さなアトミックなステートファイルと
カーネルのプロセス識別情報から復旧します。確認できない実行は保留し、
停止対象のプロセスを推測しません。

runner のアーカイブは固定バージョンを自動ダウンロードし、公式 SHA-256 ダイジェストで
検証します。デーモンの標準出力は JSONL です。設定に応じて、実行領域の cleanup 前に
runner ログを退避します。設定変更や障害状態の target の再試行には `apply` を使います。
実行ユーザーや管理ディレクトリの変更には通常停止・再起動が必要です。
通常停止では Scale Set を残し、`apply` では設定から参照されなくなったものの
削除を試みます。runner group は削除しません。

## ドキュメント

- [運用ガイド](docs/operations.ja.md): 認証、設定、実行環境、復旧、ログ、
  systemd / launchd / Lima での常駐化。
- [検証ガイド](docs/validation.ja.md): 自動テストの実行方法、明示的に実行する実環境での
  受け入れテスト、検証で保証できないこと。
- [設定サンプル](examples/config.yaml)と[設定スキーマ](schema.yaml)。

## 開発

```sh
go test -race ./...
go vet ./...
go build ./cmd/ghalet
```

GitHub Actions の依存を変更した後は `gh actions-lock` を実行し、
workflow と一緒に `.github/workflows/actions.lock` をコミットしてください。
ツールは `gh extension install github/gh-actions-lock` でインストールできます。

## 作者

[Songmu](https://github.com/Songmu)
