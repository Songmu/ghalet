# テスト

[English](testing.md)

自動テストと、公式 runner・GitHub に対して任意で実行するテストの手順です。
コマンドはリポジトリルートで実行してください。
セットアップと常駐化は[導入ガイド](../docs/getting-started.ja.md)と
[運用ガイド](../docs/operations.ja.md)を参照してください。

## 自動テスト

```sh
go test -race ./...
go vet ./...
```

CI は Linux と macOS で、実際の GitHub 登録を使わずにテストを実行します。
非 root ユーザーで実行してください。root ではプロセスのテストをスキップします。

ルートパッケージの `TestMain` は、runner helper の子プロセスを明示的に振り分けます。
CLI の子プロセステストでは、ビルドしたバイナリを通じて非公開コマンドも検証します。

パッケージをまたぐ呼び出しと、CLI・端末プロンプトの子プロセスのカバレッジは、
CI と同じコマンドで収集できます。

```sh
coverdir=$(mktemp -d)
GHALET_TEST_COVERDIR="$coverdir" \
  go test -count=1 -race -coverpkg=./... -coverprofile coverage.out -covermode atomic ./...
go tool covdata textfmt -i "$coverdir" -o coverage-subprocess.out
```

`-count=1` でキャッシュ済みのテスト結果を使わず、子プロセスのプロファイルを再生成します。
CI は両方のプロファイルを Codecov にアップロードします。収集先は絶対パスで、
テストユーザーが書き込めるディレクトリを指定してください。特権を使うパッケージ検証や、
runner guard のようにカバレッジを書き出す前に強制終了する helper は含まれません。
カバレッジが低いだけで、その処理が未テストとは限りません。

使い捨ての Linux ホスト・コンテナでは、ルートパッケージのテストバイナリをビルドし、
root で `GHALET_TEST_SYSTEMD_CREDENTIALS=1 ./ghalet-tests -test.run '^TestProtectedSystemdCredentialAccess$' -test.v`
を実行すると、実際の credential ACL と非 root の読み取りプロセスを検証できます。
systemd がサービスを管理する環境で `-test.run '^TestProtectedSystemd'` を指定すると、
`LoadCredentialEncrypted=` を使った非 root サービスも検証します。
パッケージ CI は Ubuntu で両方を実行します。使用するのはダミーの credential のみです。
本番ホストでは実行せず、開発ホストのアカウントや sudo 設定も変更しないでください。

Linux の fixture は、対象外と所属確認できた PID の消失、所属不明の PID の増減と
直前に fork した子孫に対する回数制限付き確認、既知のグループメンバーの曖昧な消失、
列挙から漏れた生存子孫、ワーカースレッドを持つゾンビリーダー、
新たに観測したゾンビ、PID の再利用を検証します。
不完全なスナップショットのプロセステストでは、fork 後に生き残る子孫を検証します。
クロスコンパイルだけでは、これらの Linux 実行時の確認を検証したことにはなりません。

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

固定した公式 runner をダウンロード・検証し、相対 symlink を含むディレクトリの
共有準備と実行ユーザーによるリンク先の読み取り、および不正な JIT 入力が出力と
診断ログでマスクされることを確認します。runner の登録やジョブ実行は行いません。
runner が要求する OS の依存パッケージと、外向き HTTPS 通信が必要です。
使い捨てのホスト・コンテナで root として実行した場合は、runner を `nobody` で起動し、
実行用の親ディレクトリで一覧取得・通過ができることを確認します。
`TestExecutionDirectoryAccessAsRunner` も root で実行し、runner のダウンロードなしで
別 UID のディレクトリアクセスとステート・ロックファイルの保護を確認できます。

## 非 root でのユーザー切り替え

`TestSudoRunnerLifecycle` は、非特権の管理プロセスと runner を別々の UID で実行します。
使い捨てのホストで、管理ユーザーを runner のプライマリグループに所属させ、
[`packaging/sudoers`](../packaging/sudoers)を基に sudo 設定を用意してください。
`/usr/bin/ghalet` はビルドしたテストバイナリの絶対パスに置き換えます。
管理ユーザーとして実行します。

```sh
GHALET_TEST_RUNNER_USER=ghalet-runner /absolute/path/ghalet-tests \
  -test.run '^TestSudoRunner' -test.v
```

パッケージの workflow はこの設定を用意し、runner の起動、識別情報を確認した停止、
管理用ファイルの保護、非公開の診断ログのマスキング付き保存、
runner が所有するファイルの削除、管理プロセスが強制終了された後のグループの停止を確認します。
合成配布物にはファイルとディレクトリを指す symlink を含め、別 UID・プライマリ GID での
展開、共有準備、アクセス確認、起動、停止、ログ保存までを検証します。

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
[`ghalet apply`](../docs/operations.ja.md#設定反映) を実行することで、旧実行時情報を
失う前に未参照 Scale Set の削除を試みます。

## 検証の限界

自動テストは偽の API を使うため、GitHub サーバー側の割り当て動作やジョブの隔離を
実証するものではありません。Organization / App の権限、複数 target のスケジューリング、
サービスの停止、再起動、スリープ・復帰は、実際の運用環境で確認してください。
[導入環境の確認](../docs/operations.ja.md#導入環境の確認)と
[信頼境界](../docs/operations.ja.md#信頼境界)も参照してください。

結果を報告する際は、実行済みの確認と未実行のシナリオを区別してください。
