# ghalet の開発者ガイド

[English](./CONTRIBUTING.md)

ghalet 自体の変更、テスト、リリースに関するガイドです。導入と運用は[利用者向けドキュメント](./docs/index.ja.md)を参照してください。

## 開発

[go.mod](./go.mod) に指定した Go を用意し、Linux または macOS を使います。リポジトリルートで、非 root ユーザーとして実行してください。

```sh
go build ./cmd/ghalet
go test -race ./...
go vet ./...
```

生成された `./ghalet` は、専用のテスト設定で実行できます。Apple Silicon の Lima VM で未公開の変更を試す場合は、`GOOS=linux GOARCH=arm64 go build -o /tmp/ghalet-linux-arm64 ./cmd/ghalet` でクロスコンパイルし、`limactl copy` でバイナリをゲストへコピーしてください。VM の構築は[サービスとしての常駐化](./docs/operations.ja.md#サービスとしての常駐化)を参照してください。

- [テスト](./contributing/testing.ja.md): 自動テストの範囲、公式 runner のスモークテスト、GitHub 上の受け入れテスト、検証の限界。
- [実行時の設計](./contributing/design.ja.md): スケジューリング、プロセス監視、復旧、永続化、マスキングの不変条件。
- [パッケージングとリリース](./contributing/releasing.ja.md): 配布物のビルド、公開、依存関係の更新。
- [ドキュメントサイト](./docs/site/README.ja.md): ローカルビルド、プレビュー、GitHub Pages への公開。

GitHub Actions の依存を変更した後は `gh actions-lock` を実行し、workflow と一緒に `.github/workflows/actions.lock` をコミットしてください。ツールは `gh extension install github/gh-actions-lock` でインストールできます。

## ドキュメントの整理方針

想定する読者に合わせて配置します。

- `README.md` / `README.ja.md` は ghalet の紹介、用途、利用者向けガイドへの入口です。開発手順はこの `CONTRIBUTING` に置き、README にはリンクだけを残します。
- `docs/` は利用者向けの導入、設定、認証、運用、トラブルシュート、対応する挙動の説明を置く場所で、公開ドキュメントサイトの正本です。
- `contributing/` は ghalet の変更、テスト、パッケージング、リリース、保守の詳しい手順を置く場所です。
- `docs/site/` は例外として、サイトのビルド基盤と保守ガイドを利用者向けドキュメントと同じ場所に置きます。サイトのページとしては公開しません。
- 各パッケージの README と GoDoc は、その責務、API の契約、実装上の不変条件を説明します。コードの近くに置いてください。
- `AGENTS.md` はリポジトリ共通の編集規約を記載し、開発手順を重複させません。

容量、drain、復旧の保証、認証情報の保護、隔離の限界など、運用判断に必要な技術的説明は利用者向けガイドに残します。実装アルゴリズム、テスト fixture、CI、リリースの仕組みは開発者向けに置きます。混在した節は分割し、有用な運用上の説明まで移動しないでください。

各手順の正本は一か所にまとめ、重複させずリンクで参照します。英語と日本語は同じ変更で更新し、言語切替と同じ言語への参照もそろえてください。サイトに関わる変更は[サイトビルド](./docs/site/README.ja.md#ローカルビルド)で検証します。
