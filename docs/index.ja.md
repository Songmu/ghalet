# ghalet ドキュメント

[English](index.md)

ghalet は Linux / macOS 向けの軽量な GitHub Actions Self-hosted runner
スーパーバイザーです。単一の Go デーモンが需要に応じてネイティブの runner プロセスを
起動し、複数のリポジトリ・Organization の実行容量を管理します。

![ghalet が単一ホスト上で、全体と target ごとの上限に従い runner をオンデマンドに起動する構成図](images/ghalet-overview.png)

専用ビルドマシン、単一用途の VM、自宅サーバーなど、信頼できる専用ホストでの運用を
想定しています。runner 間の厳密な隔離は行わないため、信頼できる workflow のみに
使用してください。

## はじめに

- [導入ガイド](getting-started.ja.md): インストール、認証、設定、最初の workflow、
  macOS での常駐化。
- [パッケージとコンテナ](distribution.ja.md): systemd を含む Linux パッケージ、
  Docker Compose、更新手順。

## 認証

- [認証のセットアップ](authentication.ja.md): GitHub CLI・PAT・GitHub App の選択と
  必要な権限の設定。
- [GitHub App のセットアップ](app-setup.ja.md): ウィザードによる App の作成と
  インストール、リモートホストや systemd サービスでの利用。
- [通常ファイルの認証情報](file-credentials.ja.md): ファイルに保存する認証情報の
  登録、交換、ロールバック。
- [systemd 用の暗号化した認証情報](credentials.ja.md): Linux のサービス用認証情報の管理。

## 運用・リファレンス

- [運用ガイド](operations.ja.md): 設定、実行ユーザー、スケーリング、復旧、ログ、
  常駐化、信頼境界。
- [コマンド一覧](../README.ja.md#コマンド): CLI の各コマンドの用途。
- [設定サンプル](../examples/config.yaml)と[設定スキーマ](../schema.yaml)。

これらの Markdown は、リポジトリ上でも
[ドキュメントサイト](https://songmu.github.io/ghalet/ja/)でも読めます。
