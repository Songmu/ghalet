# ドキュメントサイト

[English](README.md)

Songmu/tagpr と同じ Hugo / Hextra 構成を使い、リポジトリ上でも読める `docs/` の
Markdown をドキュメントの正とします。`docs/index.md` と `docs/index.ja.md` を
サイトのトップページにし、`docs/getting-started.md` と日本語版にインストールと
クイックスタートをまとめます。`docs/` 内のディレクトリ構造がサイトの構造になります。
元ファイルを日英で編集し、サイト専用の本文は重複管理しません。
ビルド時に frontmatter の付与、翻訳ペアの確認、サイト向けの Markdown リンク変換を
行います。リポジトリの README はプロジェクト紹介とドキュメントへの入口に使い、
README や設定例などのリポジトリ内ファイルへのリンクは GitHub に向けます。

## ローカルビルド

Renovate が Hugo のリリースを追跡して `.hugo-version` の更新を提案し、
Pages workflow も同じファイルを参照します。

`.hugo-version` に指定した extended 版 Hugo と、`go.mod` に指定した Go を用意し、
リポジトリルートで実行します。

```sh
bash docs/site/build.sh
```

出力先は Git 管理対象外の `site/` です。古い生成物を含まないビルドには、
空の出力先を指定してください。

```sh
bash docs/site/build.sh "$(mktemp -d)"
```

ローカル用のベースパスでビルドし、プレビューするには次を実行します。

```sh
HUGO_BASEURL=http://localhost:8000/site/ bash docs/site/build.sh
python3 -m http.server 8000 --directory .
```

英語は `http://localhost:8000/site/`、日本語は
`http://localhost:8000/site/ja/` を開いてください。

## GitHub Pages

最初のデプロイ前に、リポジトリの Settings で **Pages → Build and deployment →
Source → GitHub Actions** を一度設定します。

`pages` workflow は `docs/`、日英いずれかの README、workflow 自体を変更する PR で
ビルドします。デプロイするのは、それらのパスを変更する `main` への push と、
`main` 上での手動実行だけです。ドキュメント用タグや tagpr workflow は不要です。
公開先のベース URL は GitHub Pages から取得し、カスタムドメインにも追従します。
PR とローカルビルドのデフォルトは `https://songmu.github.io/ghalet/` です。
