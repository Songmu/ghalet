# 検証ガイド

[English](validation.md) | [日本語](validation.ja.md)

このガイドでは、ghalet の動作の根拠、自動テストと明示的に実行する実環境テストの手順、
および検証の限界を説明します。セットアップと常駐化は [README](../README.ja.md) と
[運用ガイド](operations.ja.md)を参照してください。

- [プロトコル上の契約](#プロトコル上の契約)
- [自動テスト](#自動テスト)
- [任意実行の公式 runner スモークテスト](#任意実行の公式-runner-スモークテスト)
- [明示的に実行する実環境の受け入れテスト](#明示的に実行する実環境の受け入れテスト)
- [検証の限界](#検証の限界)

## プロトコル上の契約

[go.mod](../go.mod) の `actions/scaleset` リビジョンは、公式 Listener がメッセージの
確認応答を返す前に、明示的な取得制御と確定的な統計を扱える
`listener.Scaler.Scale` コールバックを提供します。v0.4.0 タグはコールバックの契約が
異なり、そのまま置き換えてダウングレードすることはできません。

runner の入力と起動完了の扱いは、次の `actions/runner` の契約に基づきます。
以下のソース参照は 2.338.0 を使っています。

- `CommandSettings` は JIT 設定を含む `ACTIONS_RUNNER_INPUT_*` 変数を
  読み取り・除去し、シークレット引数をマスキング機構に登録します。
- `Runner.Listener run` は引数に値を露出せず JIT 設定を受け取れます。
- listener はセッション確立後に `Listening for Jobs` を出力します。

実際の起動では、この標準出力の行、または一致する SDK の JobStarted イベントで、
起動期限の計測を終了します。古いログを解析して起動完了を復元することはありません。
再起動時には永続化したステートと OS / API の識別情報を使います。
ghalet は JIT ペイロードを書き換えません。バージョン固定、ダイジェスト検証、
自己更新の扱いは[運用ガイド](operations.ja.md#直接実行と環境)を参照してください。

参照元:
[SDK listener](https://github.com/actions/scaleset/blob/e6daac702355/listener/listener.go)、
[SDK のエラーと削除](https://github.com/actions/scaleset/blob/e6daac702355/client.go)、
[runner の入力](https://github.com/actions/runner/blob/v2.338.0/src/Runner.Listener/CommandSettings.cs)、
[runner の起動](https://github.com/actions/runner/blob/v2.338.0/src/Runner.Listener/Runner.cs)。

## 自動テスト

```sh
go test -race ./...
go vet ./...
```

CI は Linux と macOS でテストを実行します。テストでは偽の GitHub API、
実際の公式 Listener、一時的な独立した runner アーカイブ、
実際の非 root サブプロセスを使います。対象範囲は次のとおりです。

| 契約 | 自動テストによる確認 |
| --- | --- |
| 設定と CLI | デフォルト値、別名、短縮 URL、厳密なキー検証と旧キーの拒否、検証順序、ディレクトリの安全性、version フラグの位置（`config_test.go`） |
| 認証情報と環境 | PAT / gh / App の設定、保護されたファイル、最小限の runner 環境、runner 入力名の大文字・小文字を区別しない拒否（`auth_test.go`、`process_test.go`、`config_test.go`） |
| シークレットのマスキング | 複数行・重複する値、共有シークレットの参照、JSONL 出力、分割書き込み時の実行中出力・アーカイブのマスキング（`auth_test.go`、`cleanup_test.go`） |
| 登録と配布 | group / Scale Set の明示的な検索、再利用、更新の無効化、ダイジェスト、独立した展開、破損キャッシュ、パストラバーサルの拒否（`auth_test.go`、`distribution_test.go`） |
| 取得とスケジューリング | 公式 Listener の確認応答・再配信、不確定または統計なしの予約、共有容量、待機枠の譲渡、実行中ジョブと割り当て競合の保護（`listener_test.go`、`daemon_test.go`） |
| ライフサイクルと停止 | 独立したジョブディレクトリ、drain 中の受付済み未処理ジョブ、起動・期限の失敗、drain 完了、実行中状態の復旧、終了と出力の順序（`daemon_test.go`） |
| ステートと復旧 | アトミック書き込み、親ディレクトリ階層の同期、ロック、旧形式・未知・破損記録、PID 再利用、読み取り可能なステートを置き換えない厳密な 1 MiB 境界（`state_test.go`、`daemon_test.go`） |
| プロセス識別と停止 | 親の変更、空・ゾンビ・古いスナップショット、Linux の列挙競合と生存スレッド、不正 PID、シグナル送信前の子孫追跡の永続化、不確かなグループの保持（`process_test.go`、`identity_linux_test.go`、`daemon_test.go`） |
| ネイティブ runner の guard | コントローラーの SIGKILL、TERM に反応しない子孫、無関係なプロセスの生存、runner の実際の結果、永続化を伴う起動ゲート、ファイルディスクリプターのクローズ（`runner_guard_test.go`） |
| cleanup とログ | 削除前の退避・出力完了、失敗時の保持と再試行、ログ行の長さ制限、保持期間、親階層のシンボリックリンク（`cleanup_test.go`） |
| 管理操作 | 設定編集後も安定した制御アドレス、GC の部分成功、コマンド終了時の動作、承認の拒否、監視完了待ちや cleanup エラーがある場合の削除対象のトランザクショナルな取り込み（`control_test.go`） |

実プロセスのテストは、誤って root ジョブを実行しないよう root ではスキップします。
非 root ユーザーで実行してください。テストが実際の GitHub 登録を自動削除したり、
信頼できない workflow を実行したりすることはありません。
ファイルシステム障害の注入は電源断のシミュレーションではありません。

## 任意実行の公式 runner スモークテスト

```sh
GHALET_TEST_OFFICIAL_RUNNER=1 \
  go test -run '^TestOfficialRunnerJITInputSmoke$' -v
```

固定した公式アーカイブをダウンロードし、リリースのダイジェストを検証して、
最小限の環境と不正な JIT 入力で `Runner.Listener run` を起動します。
そのエンコード済み入力が出力と診断ログでマスクされることを確認します。
runner の登録、ジョブの実行、実際のジョブ環境に有効な JIT シークレットが存在しないことの
実証は**行いません**。OS の実行時依存パッケージと外向き HTTPS 通信が必要です。

## 明示的に実行する実環境の受け入れテスト

使い捨て可能な信頼できるホストと、専用のリポジトリ / Scale Set のみを使ってください。
最初に、そのリポジトリのデフォルトブランチに
`.github/workflows/ghalet-integration.yaml` を配置します。
初回実行時に管理対象の実行がない状態で、リポジトリ target をちょうど 1 つ、
`minRunners: 0`、`preserveLogs: true` とし、runner の管理と integration workflow の
起動・参照ができる認証情報を設定してください。target 名の特定のプレフィックスは不要です。

```sh
GHALET_INTEGRATION_CONFIG=/absolute/path/to/dedicated/config.yaml \
  go test -run '^TestGitHubEphemeralLifecycle$' -timeout 20m -v
```

必要に応じて、起動する workflow のリビジョンを `GHALET_INTEGRATION_REF` で指定します。
このテストは、明示した Scale Set を作成または再利用し、信頼できる 2 つのジョブを
順次実行して、別々の実行アーカイブと JSON のデーモン出力を確認し、デーモンを drain
します。workflow は非 root 実行と、JIT / gh 入力変数がないことを確認します。
失敗した受け入れテストの実行時間を制限するため、drain の期限を 15 秒に設定します。
意図的に **Scale Set の登録を残します**。実行後は対話による明示承認で削除してください。

macOS ネイティブと Linux でそれぞれ実行してください。
Organization / App のインストール、全枠が待機中の 2 target、
割り当てと待機 runner 削除の同時競合、取得後の API 障害、
listener / セッションの再接続、ホストの再起動、サービスマネージャーからの停止、
Lima のスリープ・復帰については、手動でシナリオを追加します。
認証情報を含めず、SDK / runner バージョン、OS、構造化ログ、workflow 実行 URL、
実際のサーバー応答を記録してください。

## 検証の限界

偽の API で確認できるのはローカルの判断であり、GitHub サーバー側の再割り当ての
意味論ではありません。特に、次の制約があります。

- 容量ヘッダーは潜在的な需要を観測可能にしますが、実際の複数 target の可視性と
  取得上限は、実際の GitHub で受け入れテストを行う必要があります。
- `RemoveRunner` を割り当ての調停点として使います。`JobStillRunningError`、
  登録の不一致、API 失敗の場合、待機枠の回収のためにプロセスを停止しません。
  開始前の割り当て競合に対する正確な保証は、サービスに対して検証する必要があります。
  偽の API がそのエラーを返すだけでは証明できません。
- 容量ゼロと取得ゲートによってローカルの新規受付を止めます。
  期限超過やセッション終了後の取得済み処理の扱いは GitHub 側の責任であり、
  ローカルでジョブが成功したことを意味しません。
- マイクロ秒精度の macOS のプロセス識別と、Linux のブート・開始 tick による識別は、
  古い PID から保護します。収集時の競合はプロセスを隔離扱いにする前に再確認し、
  プロセス名の一致だけでシグナル送信を許可することはありません。
  切り離された子孫や、同一ユーザーの悪意あるジョブは、隔離の保証範囲外です。
- テンプレートの解析とクロスコンパイルでは、サービスの drain、
  キーチェーンアクセス、Linux での実行、VM 再起動、スリープ・復帰、
  App の権限は実証できません。

PR の検証記録では、実行済みの確認と任意の実環境シナリオを区別してください。
実際に実行していないシナリオを成功済みと記載してはいけません。
