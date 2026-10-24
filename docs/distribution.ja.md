# Linux パッケージとコンテナ

[English](./distribution.md)

リリースタグごとに amd64 / arm64 のバイナリ、`.deb` / `.rpm` パッケージ、マルチプラットフォームイメージ `ghcr.io/songmu/ghalet:TAG` を公開します。アーカイブ、`install.sh`、`go install` も利用できます。APT/DNF リポジトリは提供しません。Homebrew でのインストールと macOS のサービス連携は[導入ガイド](./getting-started.ja.md#インストール)を参照してください。

## Debian / RPM パッケージ

GitHub Releases から使用するアーキテクチャのパッケージをダウンロードし、`SHA256SUMS` と照合してください。パッケージもビルド provenance の対象です。

```sh
gh attestation verify ./PACKAGE --repo Songmu/ghalet
sudo apt install ./PACKAGE.deb
# RPM 系の場合:
sudo dnf install ./PACKAGE.rpm
```

公式 runner が要求する OS の依存パッケージと `procps` を依存関係に含みます。配布用設定は systemd が渡す暗号化 PAT を使うため、GitHub CLI は必須ではありません。登録コマンドには systemd 250 以降が必要です。Git や workflow で使用するビルドツールは別途導入してください。

パッケージは、両アーキテクチャの Ubuntu 26.04 ホストと Rocky Linux 9 コンテナでテストしています。他のディストリビューションでは導入前に互換性を確認してください。

導入時に、管理プロセス用の `ghalet` と、runner・ジョブ用の `ghalet-runner` という別々の非特権アカウントを作成します。HOME はそれぞれ `/var/lib/ghalet` と `/var/lib/ghalet-runner` です。systemd unit は `ghalet` で起動し、配布用設定は `runner.user: ghalet-runner` を指定します。コマンドを限定した sudo 設定により、runner 用ヘルパーだけを `ghalet-runner` として実行できます。root としてのコマンド実行は許可しません。管理ユーザーは runner のグループに所属し、実行ファイルの準備と停止後の回収を行います。

設定は root が所有し、`ghalet` が読み取れます。ステート、キャッシュ、保存ログは `ghalet` が所有します。暗号化した認証情報の保存元は root のみアクセス可能で、systemd がサービスのユーザーに復号した認証情報を渡します。

runner は自身の実行ディレクトリに到達するため `/var/lib/ghalet` を通過できますが、その親ディレクトリ自体は変更できません。

**本番設定の作成、サービスの自動起動の有効化、サービスの起動は行いません。**

unit は `Type=notify` を使い、`systemctl start` はデーモンの初期化が完了して管理 socket がリクエストに応答できる状態になるまで待ちます。`ghalet status --fail-on-error` の成功を保証するものではなく、target と実行データの診断は別途行います。

GitHub App の作成とサービス用の鍵の暗号化保存を行う場合:

```sh
sudo ghalet systemd app setup --scope OWNER/REPO --scale-set project
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

Organization では `--scope ORG` を使います。静的設定は変更せず、認証情報を managed ファイルに保存します。表示された target の YAML を設定に追加し、設定がなければパッケージ用の runner ユーザーとログパスを含む設定全体の案を、検証・起動前に保存してください。

表示された URL をブラウザで開いてください。SSH では[App セットアップ](./app-setup.ja.md#ssh-とブラウザの手動操作)のポート転送を使います。

PAT を使う場合は、パッケージの設定例をコピーして編集します。

```sh
sudo install -o root -g ghalet -m 0640 \
  /usr/share/ghalet/config.yaml.example /etc/ghalet/config.yaml
sudoedit /etc/ghalet/config.yaml
sudo ghalet systemd credential set default --prompt
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
```

設定の `OWNER/REPO` を置き換え、runner の管理に必要な権限を持つ PAT を用意してください。登録コマンドは参照先を managed ファイルの `defaults.credential` に書き込み、他の認証設定がない target 間で共有します。静的な認証設定が引き続き優先します。[セットアップで管理する情報](./operations.ja.md#セットアップで管理する情報)も参照してください。

パッケージのインストール・更新時にシークレットを要求せず、登録は明示的な別コマンドで行います。App キー、複数の認証情報、自動構築、ローテーションは[暗号化した認証情報](./credentials.ja.md)を参照してください。認証情報を YAML の値やシェルのコマンド引数に直接書かないでください。

古い systemd では、所有者のみ読める PAT ファイルを `defaults.credential.tokenFile` に指定する構成も使えます。[運用ガイド](./operations.ja.md#設定と認証情報)を参照してください。

unit は `StateDirectory=ghalet` と `CacheDirectory=ghalet` によって `STATE_DIRECTORY` と `CACHE_DIRECTORY` を渡し、実行データを `/var/lib/ghalet/runs`、runner のアーカイブとダイジェストを `/var/cache/ghalet/runners` に配置します。サンプル設定は保存する runner ログを `logs.root` により `/var/log/ghalet/runners` 以下に配置します。

デーモンの標準出力と標準エラーは journal に記録し、永続化・ローテーション・保持期間はホストの journald 設定に従います。

```sh
sudo journalctl -u ghalet -f
sudo ghalet status --fail-on-error --config /etc/ghalet/config.yaml
```

更新時はサービスを通常停止し、停止が不完全なら状況を確認してからパッケージを置き換えます。パッケージの更新スクリプトもサービスを停止しますが、自動では再起動しません。更新後は明示的に起動してください。既存の OS 起動時の自動起動設定は保持します。

```sh
sudo systemctl stop ghalet
# apt または dnf で新しいパッケージを導入します。
sudo systemctl start ghalet
```

削除時はサービスを停止し、自動起動を無効化します。`apt purge` を使った場合も、設定、認証情報、ステート、ログ、両方のアカウントは残します。保持された実行データを確認した後で、必要に応じて削除してください。パッケージ操作で GitHub の Scale Set を削除することはありません。

unit は `KillMode=mixed` と、停止処理全体に対する 15 分の最終期限を使います。デフォルトの drain 期限 10 分を変更する場合は、unit 側の期限も延ばしてください。停止時とクラッシュ時の後始末については、[サービスとしての常駐化](./operations.ja.md#サービスとしての常駐化)を参照してください。

## Docker Compose

コンテナ内では、ghalet と Linux runner プロセスを UID/GID 10001 で実行します。runner の依存パッケージ、Git、curl、シェルを含みますが、GitHub-hosted runner と同等の完全なツールセットは含みません。workflow 固有のツールは、このイメージを拡張して導入してください。コンテナ内で systemd は使いません。

`examples/compose.yaml` と `examples/system-config.yaml` を新しい配置用ディレクトリへコピーし、後者を `config.yaml` に変更します。`OWNER/REPO` を設定し、PAT を `credentials/token` に配置してください。Linux の Docker ホストでは、bind mount する認証情報をイメージのユーザーに合わせて準備します。

```sh
mkdir -p credentials
# シークレット管理ツールで PAT を credentials/token に書き込みます。
sudo chown -R 10001:10001 credentials
sudo chmod 0700 credentials
sudo chmod 0600 credentials/token
export GHALET_VERSION=vX.Y.Z
docker compose run --rm ghalet config validate --config /etc/ghalet/config.yaml
docker compose up -d
docker compose exec ghalet ghalet status --fail-on-error --config /etc/ghalet/config.yaml
```

`GHALET_VERSION` には公開済みのリリースタグを指定します。イメージの provenance は `gh attestation verify oci://ghcr.io/songmu/ghalet:TAG --repo Songmu/ghalet` で確認できます。他の Docker ホストでは、コンテナ内で実際に見える所有者とモードを確認してください。トークンを誰でも読める状態にすることは解決策ではありません。

設定と認証情報は読み取り専用でマウントします。名前付き volume は HOME / ステート、キャッシュ、ログを保持し、初期化時にはイメージのユーザーが所有します。同じ volume 群に対して複数のデーモンを起動しないでください。ステートを保持する場合は `docker compose down --volumes` を使わないでください。管理コマンドでも、デーモンと同じ `/etc/ghalet/config.yaml` を選択する必要があります。

Compose では、シグナル転送と孤児プロセスの回収を行う小さな init プロセスを有効にし、通常の停止 / drain のために 15 分の猶予を設けています。コンテナを作り直す場合やイメージを変更する場合は、先に通常停止してください。

```sh
docker compose stop
docker compose pull
docker compose up -d
```

`runner.drainTimeout` を延ばす場合は `stop_grace_period` も延ばしてください。強制終了、コンテナの削除、ホスト障害ではジョブが中断される場合があります。永続化したレコードは診断用に残りますが、コンテナの置き換えで実行中の runner プロセスが復旧するわけではありません。

ジョブはホストのツール環境ではなく、**この Linux コンテナ内**で実行します。macOS ジョブには引き続き macOS ネイティブの導入が必要です。コンテナ配布によって Docker actions、ジョブ / サービスコンテナ、ジョブごとの隔離に対応するわけではありません。

サンプルは Docker ソケットをマウントせず、ホストの PID namespace を共有せず、特権モードも要求しません。同じユーザーで動く信頼済みジョブは、マウントした管理用の認証情報を読めるほか、HOME、ツール、キャッシュを共有する点には注意してください。
