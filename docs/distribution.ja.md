# Linux パッケージとコンテナ

[English](distribution.md) | [日本語](distribution.ja.md)

リリースタグごとに amd64 / arm64 のバイナリ、`.deb` / `.rpm` パッケージ、
マルチプラットフォームイメージ `ghcr.io/songmu/ghalet:TAG` を公開します。
既存のアーカイブ、`install.sh`、`go install` も引き続き利用できます。
この配布設定には APT/DNF リポジトリや Homebrew のサービス連携は含みません。

## Debian / RPM パッケージ

GitHub Releases から使用するアーキテクチャのパッケージをダウンロードし、
`SHA256SUMS` と照合してください。パッケージもビルド provenance の対象です。

```sh
gh attestation verify ./PACKAGE --repo Songmu/ghalet
sudo apt install ./PACKAGE.deb
# RPM 系の場合:
sudo dnf install ./PACKAGE.rpm
```

公式 runner が要求する OS の依存パッケージと `procps` を依存関係に含みます。
配布用設定は systemd が渡す暗号化 PAT を使うため、GitHub CLI は必須ではありません。
登録コマンドには systemd 250 以降が必要です。
Git や workflow で使用するビルドツールは別途導入してください。
パッケージのスモークテストは、両アーキテクチャの Ubuntu 26.04 ホストと
Rocky Linux 9 コンテナを対象としています。他のディストリビューションでは
別途互換性の確認が必要です。

導入時に、非特権の `ghalet-runner` アカウント、その HOME となる
`/var/lib/ghalet-runner`、保存用ディレクトリ、systemd unit を作成します。
管理プロセスはユーザー切り替えのため root で起動し、配布用設定の
`runner.user: ghalet-runner` により runner とジョブを非 root で実行します。
設定、管理用認証情報、キャッシュ、保存ログは root のみアクセス可能です。
runner は自身の実行ディレクトリに到達するため `/var/lib/ghalet` を通過できますが、
その親ディレクトリ自体は変更できません。
**本番設定の作成、サービスの自動起動の有効化、サービスの起動は行いません。**

```sh
sudo install -o root -g root -m 0600 \
  /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
sudoedit /etc/ghalet/config.yaml
sudo ghalet credential set default --prompt
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

設定の `OWNER/REPO` を置き換え、runner の管理に必要な権限を持つ PAT を用意してください。
登録コマンドは参照先を `defaults.credential` に書き込み、`credential` を省略した
target 間で共有します。パッケージのインストール・更新時にシークレットを要求せず、
登録は明示的な別コマンドで行います。App キー、複数の認証情報、自動構築、
ローテーションは[暗号化した認証情報](credentials.ja.md)を参照してください。
認証情報を YAML の値やシェルのコマンド引数に直接書かないでください。
古い systemd では、所有者のみ読める PAT ファイルを
`defaults.credential.tokenFile` に指定する構成も使えます。
[運用ガイド](operations.ja.md#設定と認証情報)を参照してください。

サンプル設定は、実行データと runner のメタデータを `/var/lib/ghalet`、
ダウンロードを `/var/cache/ghalet`、保存する runner ログを `cleanup.logRoot` により
`/var/log/ghalet/runners` 以下に配置します。デーモンの標準出力と標準エラーは
journal に記録し、永続化・ローテーション・保持期間はホストの journald 設定に従います。

```sh
sudo journalctl -u ghalet -f
sudo ghalet doctor --config /etc/ghalet/config.yaml
```

更新時はサービスを通常停止し、停止が不完全なら状況を確認してからパッケージを置き換えます。
パッケージの更新スクリプトもサービスを停止しますが、自動では再起動しません。
更新後は明示的に起動してください。既存の OS 起動時の自動起動設定は保持します。

従来の `ghalet` ユーザーを共用するパッケージから更新する場合は、先に既存の実行を
drain し、後始末を完了してください。既存設定は書き換えないため、root で動く新しい
サービスを起動する前に `/etc/ghalet/config.yaml` に `runner.user: ghalet-runner` を
追加してください。未指定の場合は root でのジョブ実行を拒否して起動に失敗します。
管理用認証ファイルは所有者を `root:root`、権限を `0600` に変更してください。
従来の `ghalet` アカウントと保持されたファイルは自動削除しません。

```sh
sudo systemctl stop ghalet
# apt または dnf で新しいパッケージを導入します。
sudo systemctl start ghalet
```

削除時はサービスを停止し、自動起動を無効化します。`apt purge` を使った場合も、
設定、認証情報、ステート、ログ、runner アカウントは残します。
保持された実行データを確認した後で、必要に応じて削除してください。
パッケージ操作で GitHub の Scale Set を削除することはありません。

unit は `KillMode=mixed` と、停止処理全体に対する 15 分の最終期限を使います。
デフォルトの drain 期限 10 分を変更する場合は、unit 側の期限も延ばしてください。
停止時とクラッシュ時の後始末については、
[サービスとしての常駐化](operations.ja.md#サービスとしての常駐化)を参照してください。

## Docker Compose

コンテナ内では、ghalet と Linux runner プロセスを UID/GID 10001 で実行します。
runner の依存パッケージ、Git、curl、シェルを含みますが、
GitHub-hosted runner と同等の完全なツールセットは含みません。
workflow 固有のツールは、このイメージを拡張して導入してください。
コンテナ内で systemd は使いません。

`examples/compose.yaml` と `examples/system-config.yaml` を新しい配置用ディレクトリへ
コピーし、後者を `config.yaml` に変更します。`OWNER/REPO` を設定し、
PAT を `credentials/token` に配置してください。Linux の Docker ホストでは、
bind mount する認証情報をイメージのユーザーに合わせて準備します。

```sh
mkdir -p credentials
# シークレット管理ツールで PAT を credentials/token に書き込みます。
sudo chown -R 10001:10001 credentials
sudo chmod 0700 credentials
sudo chmod 0600 credentials/token
export GHALET_VERSION=vX.Y.Z
docker compose run --rm ghalet config validate --config /etc/ghalet/config.yaml
docker compose up -d
docker compose exec ghalet ghalet doctor --config /etc/ghalet/config.yaml
```

`GHALET_VERSION` には公開済みのリリースタグを指定します。
イメージの provenance は
`gh attestation verify oci://ghcr.io/songmu/ghalet:TAG --repo Songmu/ghalet`
で確認できます。他の Docker ホストでは、コンテナ内で実際に見える所有者とモードを
確認してください。トークンを誰でも読める状態にすることは解決策ではありません。

設定と認証情報は読み取り専用でマウントします。名前付き volume は HOME / ステート、
キャッシュ、ログを保持し、初期化時にはイメージのユーザーが所有します。
同じ volume 群に対して複数のデーモンを起動しないでください。
ステートを保持する場合は `docker compose down --volumes` を使わないでください。
管理コマンドでも、デーモンと同じ `/etc/ghalet/config.yaml` を選択する必要があります。

Compose では、シグナル転送と孤児プロセスの回収を行う小さな init プロセスを有効にし、
通常の停止 / drain のために 15 分の猶予を設けています。
コンテナを作り直す場合やイメージを変更する場合は、先に通常停止してください。

```sh
docker compose stop
docker compose pull
docker compose up -d
```

`runner.drainTimeout` を延ばす場合は `stop_grace_period` も延ばしてください。
強制終了、コンテナの削除、ホスト障害ではジョブが中断される場合があります。
永続化したレコードは診断用に残りますが、コンテナの置き換えで実行中の
runner プロセスが復旧するわけではありません。

ジョブはホストのツール環境ではなく、**この Linux コンテナ内**で実行します。
macOS ジョブには引き続き macOS ネイティブの導入が必要です。
コンテナ配布によって Docker actions、ジョブ / サービスコンテナ、
ジョブごとの隔離に対応するわけではありません。
サンプルは Docker ソケットをマウントせず、ホストの PID namespace を共有せず、
特権モードも要求しません。同じユーザーで動く信頼済みジョブは、マウントした管理用の
認証情報を読めるほか、HOME、ツール、キャッシュを共有する点には注意してください。

## 配布物のビルド

`release-build.yaml` は、Linux / macOS のアーカイブを goxz、
Linux パッケージを nFPM で生成します。リリース用ファイルは `dist` に集約し、
パッケージ生成後、その配下の全ファイルについて `SHA256SUMS` を一度だけ生成します。
manifest 自身は対象から除外します。
`packaging/build.sh` はアーカイブから同じ Linux バイナリを取り出し、
`examples/ghalet.service` を元にパッケージ用 unit を生成して、
コンテナのビルド用にはバイナリとライセンス文書だけを用意します。

nFPM のバージョンは [`packaging/nfpm.version`](../packaging/nfpm.version) に固定します。
Renovate が nFPM と組み込み runner の更新を毎週提案します。
Go modules、GitHub Actions、Docker イメージは引き続き Dependabot で管理します。
自動更新 PR を作成するには、[Renovate GitHub App](https://github.com/apps/renovate) の
インストール設定で、このリポジトリを有効にしてください。

```sh
go install github.com/Songmu/goxz/cmd/goxz@v0.13.0
go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$(cat packaging/nfpm.version)"
VERSION=$(sed -n 's/^const Version = "\([^"]*\)"$/v\1/p' version.go)
test -n "$VERSION"
goxz -pv "$VERSION" -os linux,darwin -arch amd64,arm64 -static \
  -d /tmp/ghalet-release ./cmd/ghalet
sh packaging/build.sh "$VERSION" /tmp/ghalet-release /tmp/ghalet-release /tmp/ghalet-binaries
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-context binaries=/tmp/ghalet-binaries .
```

Dockerfile は、バイナリとライセンス文書だけを含む明示的な名前付き context を使います。
`.dockerignore` は、それ以外のチェックアウト内容を除外します。
ローカル設定、認証情報、worktree、Git メタデータも対象です。
リリース時には、パッケージをアーカイブや共通の checksum / provenance と共に公開し、
別のジョブで GHCR イメージの公開と attestation を行います。
独立した `test-packaging.yaml` はパッケージとイメージをビルド・検証しますが、
公開は行わず、レジストリの認証情報も受け取りません。
そのバージョンは、すでに tagpr の更新対象となっている `version.go` から取得します。

初回のイメージ公開後、リポジトリ所有者は GHCR のパッケージ設定を確認し、
匿名で pull できるようにパッケージを Public にする必要があります。
リポジトリを公開しても、新規パッケージが自動的に Public になるわけではありません。
