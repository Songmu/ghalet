# パッケージングとリリース

[English](releasing.md)

ghalet の配布物のビルドと公開に関するガイドです。
公開済みのパッケージやコンテナの導入・更新は
[利用者向けガイド](../docs/distribution.ja.md)を参照してください。

## 配布物のビルド

[`release-build.yaml`](../.github/workflows/release-build.yaml) は、
Linux / macOS のアーカイブを goxz、Linux パッケージを nFPM で生成します。
リリース用ファイルは `dist` に集約し、パッケージ生成後、その配下の全ファイルについて
`SHA256SUMS` を一度だけ生成します。manifest 自身は対象から除外します。
[`packaging/build.sh`](../packaging/build.sh) はアーカイブから同じ Linux バイナリを取り出し、
`examples/ghalet.service` を元にパッケージ用 unit を生成して、
コンテナのビルド用にはバイナリとライセンス文書だけを用意します。

リポジトリルートで実行してください。

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

## 公開と検証

リリース時には、パッケージをアーカイブや共通の checksum / provenance と共に公開し、
別のジョブで GHCR イメージの公開と attestation を行います。
バージョンは tagpr の更新対象である `version.go` から取得します。
独立した [`test-packaging.yaml`](../.github/workflows/test-packaging.yaml) は
パッケージとイメージをビルド・検証しますが、公開は行わず、
レジストリの認証情報も受け取りません。
検証範囲と限界は[テスト](testing.ja.md#自動テスト)を参照してください。

初回のイメージ公開後、リポジトリ所有者は GHCR のパッケージ設定を確認し、
匿名で pull できるようにパッケージを Public にする必要があります。
リポジトリを公開しても、新規パッケージが自動的に Public になるわけではありません。

## 依存関係の更新

nFPM のバージョンは [`packaging/nfpm.version`](../packaging/nfpm.version) に固定します。
Renovate が nFPM と組み込み runner の更新を毎週提案します。
runner の更新では公式の安定版を確認し、組み込みのデフォルトとサンプルを
まとめて更新する PR を作成します。変更は通常の CI で検証します。
ドキュメントサイトの
[`docs/site/.hugo-version`](../docs/site/.hugo-version) も Renovate が追跡します。
Go modules、GitHub Actions、Docker イメージは Dependabot で管理します。
自動更新 PR を作成するには、[Renovate GitHub App](https://github.com/apps/renovate) の
インストール設定で、このリポジトリを有効にしてください。
