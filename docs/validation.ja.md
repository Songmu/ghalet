# 検証ガイド

[English](validation.md) | [日本語](validation.ja.md)

自動テストと、公式 runner・GitHub に対して任意で実行するテストの手順です。
セットアップと常駐化は [README](../README.ja.md) と
[運用ガイド](operations.ja.md)を参照してください。

## 自動テスト

```sh
go test -race ./...
go vet ./...
```

CI は Linux と macOS で、実際の GitHub 登録を使わずにテストを実行します。
非 root ユーザーで実行してください。root ではプロセスのテストをスキップします。

`test-packaging.yaml` は amd64 / arm64 の検証用アーカイブと deb/rpm をビルドし、
Ubuntu 26.04 ホストと Rocky Linux 9 コンテナで導入・再導入・削除を確認します。
Docker イメージも公開せずにビルドします。Ubuntu では導入した unit を検証し、
パッケージ操作後にサービスが起動していないことも確認します。
さらに、イメージ内で配布用設定を検証し、任意実行の公式 runner スモークテストを実行します。
実際の GitHub ジョブや systemd 管理下での drain は検証しません。
CI では、ホスト側で認証付きの GitHub API アクセスにより公式 runner のダイジェストを取得し、
`GHALET_TEST_RUNNER_DIGEST_FILE` 用に読み取り専用でマウントします。
コンテナ内でのアーカイブ検証は維持し、API トークンは渡しません。

## 任意実行の公式 runner スモークテスト

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

固定した公式 runner をダウンロード・検証し、不正な JIT 入力が出力と診断ログで
マスクされることを確認します。runner の登録やジョブ実行は行いません。
runner が要求する OS の依存パッケージと、外向き HTTPS 通信が必要です。
使い捨てのホスト・コンテナで root として実行した場合は、runner を `nobody` で起動し、
旧 `0711` の親ディレクトリでのエラー再現と、移行後にその起動チェックを通過することを確認します。
`TestExecutionDirectoryAccessAsRunner` も root で実行し、runner のダウンロードなしで
別 UID のディレクトリアクセスとステート・ロックファイルの保護を確認できます。

## 明示的に実行する実環境の受け入れテスト

このテストは GitHub 上で実際のジョブを実行します。使い捨て可能な信頼できるホストと、
専用のリポジトリ / Scale Set を使い、既存の管理対象の実行がない状態で始めてください。

1. リポジトリのデフォルトブランチに[受け入れテスト用 workflow](../.github/workflows/ghalet-integration.yaml)
   を配置します。
2. リポジトリ target をちょうど 1 つ、`minRunners: 0`、`logs.preserve: true`
   とする設定を用意します。
3. runner の管理と、受け入れテスト用 workflow の起動・参照ができる認証情報を用意します。

```sh
GHALET_INTEGRATION_CONFIG=/absolute/path/to/dedicated/config.yaml \
  go test -run '^TestGitHubEphemeralLifecycle$' -timeout 20m -v
```

必要に応じて、起動する workflow のリビジョンを `GHALET_INTEGRATION_REF` で指定します。
テストは Scale Set を作成または再利用し、2 つのジョブを順次実行して、
別々の実行ログを確認し、15 秒の期限でデーモンを drain します。
**Scale Set の登録を残してデーモンを停止します。** 実行後は専用の Scale Set を
GitHub 上で手動削除してください。通常運用では、target を設定から外して
[`ghalet apply`](operations.ja.md#設定反映) を実行することで、旧実行時情報を
失う前に未参照 Scale Set の削除を試みます。

## 検証の限界

自動テストは偽の API を使うため、GitHub サーバー側の割り当て動作やジョブの隔離を
実証するものではありません。Organization / App の権限、複数 target のスケジューリング、
サービスの停止、再起動、スリープ・復帰は、実際の運用環境で確認してください。
[信頼境界](operations.ja.md#信頼境界)も参照してください。

結果を報告する際は、実行済みの確認と未実行のシナリオを区別してください。
