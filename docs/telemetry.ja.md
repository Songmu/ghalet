# OpenTelemetry メトリクス

[English](telemetry.md)

ghalet は、OpenTelemetry Collector などの OTLP 対応 receiver にデーモンの
メトリクスを送信できます。**デフォルトでは無効**で、provider、定期収集、
送信、テレメトリ用 listener は起動しません。
既存の JSON ログと管理コマンドの挙動は変わりません。
計測はデーモンの初期化・復旧後に開始します。
オフラインの `gc` を含む、単独の管理コマンドではテレメトリを初期化しません。

## メトリクスの有効化

設定ファイルに次の項目を追加して、メトリクスを有効にします。

```yaml
telemetry:
  metrics:
    exporter: otlp
    endpoint: http://127.0.0.1:4318/v1/metrics
```

`endpoint` は path を含む完全なメトリクス URL です。それ以外の設定は任意なので、
必要になるまでは既定値を使い、変更する場合は[詳細設定](#詳細設定)を参照してください。
URL や resource 属性には認証情報を含めず、認証には標準ヘッダーの
[環境変数](#環境変数)を使ってください。

HTTP と binary protobuf による OTLP のみに対応します。ghalet から外向きに通信し、
メトリクス用ポートは開きません。Collector を同じホストに置く必要もありません。
トレース、OTLP ログ、メトリクスの標準出力、Prometheus endpoint は未実装です。

設定ファイルの変更は
`ghalet apply --config /path/to/config.yaml` で反映できます。

テレメトリだけの変更では runner の drain や target listener の置き換えを行いません。
他の設定も同時に変更した場合は、通常の apply の反映処理を行います。
新しい provider は drain 前に準備し、反映までは収集・送信しません。
準備に失敗した場合は旧設定・provider を維持します。
旧 provider の終了時には期限付きで最後の送信を試みますが、配信失敗はログに記録するだけで、
新設定をロールバックしません。

## 詳細設定

サービスの識別情報、resource 属性、送信間隔、タイムアウトは必要に応じて変更できます。
上書きしたい項目だけを指定してください。

```yaml
telemetry:
  serviceName: ghalet
  resourceAttributes:
    deployment.environment.name: production
  metrics:
    exporter: otlp
    endpoint: http://127.0.0.1:4318/v1/metrics
    interval: 60s
    timeout: 10s
```

`interval` と `timeout` は設定ファイルの期間記法を使い、`1ms` 以上を指定します。
`timeout` は収集・送信と HTTP リクエストの両方の期限を設定します。
省略時の SDK デフォルトはそれぞれ 30 秒、10 秒です。resource 属性の値は文字列です。

### 環境変数

設定ファイルの代わりに、`ghalet run` を起動するプロセスの環境変数でも設定できます。

```sh
OTEL_METRICS_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:4318/v1/metrics \
ghalet run --config /path/to/config.yaml
```

systemd ではサービスのドロップインで環境変数を設定し、
`systemctl daemon-reload` 後にサービスを再起動してください。launchd では
サービスの `EnvironmentVariables` に設定して再読込します。
シェルの環境変数を変えても、稼働中のデーモンのテレメトリ設定は変わりません。
環境変数の変更にはサービスの再起動が必要です。
[サービスとしての常駐化](operations.ja.md#サービスとしての常駐化)を参照してください。

空でない環境変数は対応するファイル設定より優先します。endpoint の優先順位は、
メトリクス固有の環境変数、全体の環境変数、設定ファイル、SDK デフォルトの順です。
`OTEL_METRICS_EXPORTER=none` でファイルから有効にした計測も無効化でき、
`OTEL_SDK_DISABLED=true` は両方の設定より優先します。
ファイルの `serviceName` は `resourceAttributes.service.name` より優先します。
環境変数の resource 属性は同名のファイル属性を上書きし、
サービス名には `OTEL_SERVICE_NAME` が最優先されます。

| 環境変数 | 挙動 |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | `otlp` で有効、`none` で無効。未設定・空ではファイル設定を使い、デフォルトは無効です。それ以外は起動エラーです。 |
| `OTEL_SDK_DISABLED` | 大文字・小文字を区別しない `true` で無効化し、exporter の設定より優先します。 |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | path を含むメトリクスの完全な URL。全体の endpoint 設定より優先します。 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | 基点の URL に `/v1/metrics` を付加します。環境変数・ファイルの endpoint がなければ、この Go exporter のデフォルトは `https://localhost:4318/v1/metrics` です。 |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `OTEL_EXPORTER_OTLP_PROTOCOL` より優先。未設定時は `http/protobuf` で、有効時にそれ以外を指定すると拒否します。 |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | 認証ヘッダー。`OTEL_EXPORTER_OTLP_HEADERS` より優先します。 |
| `OTEL_METRIC_EXPORT_INTERVAL` | 送信間隔、ミリ秒単位。SDK のデフォルトは 60000 です。 |
| `OTEL_METRIC_EXPORT_TIMEOUT` | 収集・送信の期限、ミリ秒単位。SDK のデフォルトは 30000 です。 |
| `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` | HTTP リクエストの期限、ミリ秒単位。`OTEL_EXPORTER_OTLP_TIMEOUT` より優先し、デフォルトは 10000 です。 |
| `OTEL_RESOURCE_ATTRIBUTES` | `deployment.environment.name=production` などの明示的な resource 属性。秘密情報は含めないでください。 |
| `OTEL_SERVICE_NAME` | ファイル・環境変数の resource にあるサービス名より優先します。デフォルトは `ghalet` です。 |

SDK の標準 OTLP/HTTP 設定による圧縮、TLS 証明書、メトリクスの temporality にも
対応します。[Go exporter のリファレンス](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp)
を参照してください。endpoint だけを指定しても有効にはならず、
明示的な opt-in が必要です。

[service resource 規約](https://opentelemetry.io/docs/specs/semconv/resource/service/)に沿って、
resource には ghalet の `service.version` と、デーモン起動ごとに新しい
`service.instance.id` を付けます。この 2 属性はファイル・環境変数の属性から
継承せず、ghalet が設定します。
`apply` でテレメトリを再構成しても `service.instance.id` は維持します。
コントローラーの環境変数は runner のジョブへ自動ではコピーされません。
OTLP 認証ヘッダーの環境変数は `runner.env` に設定できません。

## メトリクス

意味が一致する `cicd.worker.count` と `cicd.system.errors` は、
[CI/CD semantic conventions](https://opentelemetry.io/docs/specs/semconv/cicd/cicd-metrics/)に
合わせています。現在は Release Candidate の規約です。resource と instrumentation scope に
semantic schema `1.43.0` を設定します。それ以外は
[一般のメトリクス規約](https://opentelemetry.io/docs/specs/semconv/general/metrics/)に沿った
ghalet 固有の instrument として扱います。
target ごとの計測値には、`status` と同じ導出キーである `ghalet.target.id` を付けます。

| Instrument | 型 | 単位 | 意味 |
| --- | --- | --- | --- |
| `ghalet.capacity.limit` | Gauge | `{slot}` | ホスト全体の runner 容量上限。 |
| `ghalet.capacity.occupied` | Gauge | `{slot}` | 診断上保持する予約枠を含む、占有中の容量。 |
| `cicd.worker.count` | 非単調の sum | `{worker}` | このデーモンが管理する、オンラインと確認できた worker 数。target と `cicd.worker.state`（`available` または `busy`）で分類します。 |
| `cicd.system.errors` | Counter | `{error}` | コントローラーの起動・cleanup 失敗。`cicd.system.component=controller`、`error.type`、`ghalet.operation` を付けます。 |
| `ghalet.execution.count` | 非単調の sum | `{execution}` | 保持中の実行記録数。target と排他的な `ghalet.runner.state` で分類します。 |
| `ghalet.runner.start.attempts` | Counter | `{attempt}` | warm pool 用を含む、runner の予約試行数。 |
| `ghalet.runner.startup.duration` | Histogram | `s` | 予約から最初の ready 観測までの時間。起動成功ごとに 1 回記録します。 |

worker 数には、ready・登録済みで、プロセス確認に失敗していない runner だけを含めます。
`idle` は `available`、`busy` は `busy` に対応します。準備中・停止中・cleanup 中・
不確実な記録は除外します。保持中の実行記録から `offline` を推測したり、
リモートの全 worker を棚卸ししたりはしません。

system errors の対象は `ghalet.operation=runner.start`（ready 前の失敗を起動ごとに
1 回、キャンセルを除く）と `runner.cleanup`（再試行を含む失敗試行ごと）です。
すべてのデーモンエラーや workflow の失敗を網羅するものではありません。
低カーディナリティの `error.type` として `timeout`、`permission_denied`、
`not_found`、`canceled`（cleanup のみ）、`_OTHER` を使い、エラー本文は使いません。

実行記録の状態は `uncertain`、`preparing`、`cleaning`（プロセス停止済み）、
`exiting`、`busy`、`idle`（ready）の順で判定し、まだ ready でなければ
`preparing` とします。状態は排他的です。cleanup 待ちの記録も含むため、
合計は占有容量と一致するとは限りません。該当する実行のない状態の series は出しません。

起動時間 histogram の bucket 境界は、秒単位で
`0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600` です。
復旧した実行や、テレメトリ無効時に開始した起動の計測値は再構成しません。
すでに計測中の起動は元のタイマーを維持し、完了時点で有効な provider に結果を記録します。
無効時に観測した結果を、後から再送することはありません。
runner の ready や終了は workflow の結果ではなく、workflow 成功率は推測しません。
Scale Set の通知には workflow run ID・workflow 参照・ジョブ結果・時刻が含まれますが、
対象はその Scale Set のジョブであり、workflow の全ジョブとは限りません。
そのため runner 起動時間を `cicd.pipeline.run.duration`、実行記録数を
`cicd.pipeline.run.active` としては扱いません。これらの計測には、各ジョブを
pipeline run と見なすのではなく、workflow 単位の情報源が必要です。

counter と histogram は、デーモンの再起動またはテレメトリ設定変更による provider の
置き換えでリセットします。テレメトリ設定を変えずに他の設定を `apply` する場合はリセットしません。
counter は最初の計測後に現れます。削除された runner 状態の series は次の収集で消えますが、
過去の counter・histogram の series は provider の存続中に残ることがあります。
instrument ごとに、SDK の overflow series（`otel.metric.overflow=true`）を含む
2000 series を上限とします。
run ID、runner 名、path、URL、エラー本文はメトリクスのラベルに含めません。

## 送信失敗と停止

収集はメモリー内のスナップショットを読み、GitHub への問い合わせや runner ファイルの
読込は行いません。SDK での観測やネットワーク送信の前にデーモンの共有 lock を解放します。
送信は SDK の期限付きでバックグラウンド実行し、送信の再試行ループや永続キューは設けません。
送信に失敗しても runner の処理を失敗させず、`doctor` の診断にも加えません。
SDK は汎用の送信エラーを error handler（デフォルトは stderr）へ報告します。
そのエラーには Collector の URL、認証情報、応答本文を含めません。

worker の停止後、3 秒の期限で最後の送信を試みます。
失敗はログに記録しますが、デーモンの終了結果は変えません。
テレメトリはベストエフォートであり、実行の監査記録ではありません。
下流への再試行や永続バッファーが必要な場合は Collector を使ってください。
