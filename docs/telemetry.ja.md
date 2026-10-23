# OpenTelemetry メトリクス

[English](telemetry.md)

ghalet は、OpenTelemetry Collector などの OTLP 対応 receiver にデーモンの
メトリクスを送信できます。**デフォルトでは無効**で、provider、定期収集、
送信、テレメトリ用 listener は起動しません。
既存の JSON ログと管理コマンドの挙動は変わりません。
計測はデーモンの初期化・復旧後に開始します。
オフラインの `gc` を含む、単独の管理コマンドではテレメトリを初期化しません。

## メトリクスの有効化

`ghalet run` を起動するプロセスの環境変数を設定します。

```sh
OTEL_METRICS_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:4318/v1/metrics \
ghalet run --config /path/to/config.yaml
```

HTTP と binary protobuf による OTLP のみに対応します。ghalet から外向きに通信し、
メトリクス用ポートは開きません。Collector を同じホストに置く必要もありません。
トレース、OTLP ログ、メトリクスの標準出力、Prometheus endpoint は未実装です。

systemd ではサービスのドロップインで環境変数を設定し、
`systemctl daemon-reload` 後にサービスを再起動してください。launchd では
サービスの `EnvironmentVariables` に設定して再読込します。
シェルの環境変数を変えたり `ghalet apply` を実行したりしても、
稼働中のデーモンのテレメトリ設定は変わりません。
[サービスとしての常駐化](operations.ja.md#サービスとしての常駐化)を参照してください。

| 環境変数 | 挙動 |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | 未設定・空・`none` では無効。`otlp` の明示で有効になります。それ以外は起動エラーです。 |
| `OTEL_SDK_DISABLED` | 大文字・小文字を区別しない `true` で無効化し、exporter の設定より優先します。 |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | path を含むメトリクスの完全な URL。全体の endpoint 設定より優先します。 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | 基点の URL。デフォルトは `http://localhost:4318` で、`/v1/metrics` を付加します。 |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `OTEL_EXPORTER_OTLP_PROTOCOL` より優先。未設定時は `http/protobuf` で、有効時にそれ以外を指定すると拒否します。 |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | 認証ヘッダー。`OTEL_EXPORTER_OTLP_HEADERS` より優先します。 |
| `OTEL_METRIC_EXPORT_INTERVAL` | 送信間隔、ミリ秒単位。SDK のデフォルトは 60000 です。 |
| `OTEL_METRIC_EXPORT_TIMEOUT` | 収集・送信の期限、ミリ秒単位。SDK のデフォルトは 30000 です。 |
| `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` | HTTP リクエストの期限、ミリ秒単位。`OTEL_EXPORTER_OTLP_TIMEOUT` より優先し、デフォルトは 10000 です。 |
| `OTEL_RESOURCE_ATTRIBUTES` | `deployment.environment.name=production` などの明示的な resource 属性。秘密情報は含めないでください。 |
| `OTEL_SERVICE_NAME` | デフォルトのサービス名 `ghalet` を上書きします。 |

SDK の標準 OTLP/HTTP 設定による圧縮、TLS 証明書、メトリクスの temporality にも
対応します。[Go exporter のリファレンス](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp)
を参照してください。endpoint だけを指定しても有効にはならず、
明示的な opt-in が必要です。

resource には ghalet の `service.version` と、デーモン起動ごとに新しい
`service.instance.id` を付けます。この 2 属性は `OTEL_RESOURCE_ATTRIBUTES` から
継承せず、ghalet が設定します。
コントローラーの環境変数は runner のジョブへ自動ではコピーされません。
OTLP 認証ヘッダーの環境変数は `runner.env` に設定できません。

## メトリクス

これらは ghalet 固有の instrument 名であり、標準の CI/CD メトリクスではありません。
target ごとの計測値には、`status` と同じ導出キーである `ghalet.target.id` を付けます。
runner 数だけは `ghalet.runner.state` も持ちます。

| Instrument | 型 | 単位 | 意味 |
| --- | --- | --- | --- |
| `ghalet.capacity.limit` | Gauge | `{slot}` | ホスト全体の runner 容量上限。 |
| `ghalet.capacity.occupied` | Gauge | `{slot}` | 診断上保持する予約枠を含む、占有中の容量。 |
| `ghalet.runner.count` | 非単調の sum | `{runner}` | 保持中の実行記録数。target と排他的な状態で分類します。 |
| `ghalet.runner.start.attempts` | Counter | `{attempt}` | warm pool 用を含む、runner の予約試行数。 |
| `ghalet.runner.start.failures` | Counter | `{failure}` | ready 前に失敗した試行数。キャンセルは除外します。 |
| `ghalet.runner.startup.duration` | Histogram | `s` | 予約から最初の ready 観測までの時間。起動成功ごとに 1 回記録します。 |
| `ghalet.cleanup.failures` | Counter | `{failure}` | cleanup の失敗試行数。再試行も失敗ごとに加算します。 |

runner の状態は `uncertain`、`preparing`、`cleaning`（プロセス停止済み）、
`exiting`、`busy`、`idle`（ready）の順で判定し、まだ ready でなければ
`preparing` とします。状態は排他的です。cleanup 待ちの記録も含むため、
合計は占有容量と一致するとは限りません。該当する実行のない状態の series は出しません。

起動時間 histogram の bucket 境界は、秒単位で
`0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600` です。
復旧した実行の起動計測値は再構成しません。
runner の ready や終了は workflow の結果ではなく、workflow 成功率は推測しません。

counter と histogram は `apply` ではなくデーモンの再起動でリセットします。
counter は最初の計測後に現れます。削除された runner 状態の series は次の収集で消えますが、
過去の counter・histogram の series はデーモンの存続中に残ることがあります。
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
