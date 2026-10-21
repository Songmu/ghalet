# 通常ファイルへの認証情報の保存

[English](file-credentials.md)

`ghalet credential set` は Linux / macOS で名前付きの PAT または GitHub App 秘密鍵を
登録します。systemd や root は不要です。保存先は**暗号化されていない所有者限定のファイル**で、
キーチェーンや暗号化 vault ではありません。GitHub の権限と認証情報の作成は
[認証のセットアップ](authentication.ja.md)、パッケージのサービスでの暗号化保存は
[systemd の認証情報](credentials.ja.md)を参照してください。

## 認証情報の登録

先に[設定ファイル](getting-started.ja.md#クイックスタート)を作成し、
デーモンを起動するユーザーで実行します。

```sh
ghalet credential set default --prompt
ghalet credential set default --file /secure/pat-file
ghalet credential set default --stdin < /secure/pat-file
```

入力方法は一つだけ選びます。デフォルトは `pat` で、`--prompt` は対話端末から
エコーなしで入力し、タイムアウトは 2 分です。`--file` には所有者だけがアクセスできる
通常ファイルが必要で、入力ファイルは変更しません。入力の上限は 1 MiB です。
シークレットを引数、シェル履歴、YAML、runner の設定に書かないでください。

App では暗号化されていない RSA PEM 秘密鍵（PKCS#1 または PKCS#8）を取り込みます。

```sh
ghalet credential set app-work --type github-app \
  --client-id APP_CLIENT_ID --file /secure/app.pem
```

保存先は `<StateDir>/credentials/NAME.pat` または `NAME.pem` です。
ディレクトリは `0700`、ファイルは `0600` にします。
StateDir は `STATE_DIRECTORY`、未設定なら `$XDG_STATE_HOME/ghalet` または
`~/.local/state/ghalet` に従います。デーモンと同じ環境で登録してください。
認証ファイルは `runs/`・`logs/` と分離し、`ghalet gc` では削除しません。

選択した設定に対応する [managed ファイル](operations.ja.md#セットアップで管理する情報)の
`defaults.credential` に、`type: pat` / `tokenFile`、
または `type: github-app` / `clientId` / `privateKeyFile` を保存します。
パスは絶対パスです。デフォルト設定は `~/.config/ghalet/config.yaml` で、
`--config PATH` で変更できます。`--client-id` を省略すると、既存の App 共通設定の
client ID を再利用します。各 App target の `installationId` は別途必要です。
静的な設定ファイルは変更せず、そこに明示した共通設定と target の認証設定を優先します。

`--no-default` は managed の共通設定を変更せずに保存します。表示されたパスを利用したい target に
指定してください。名前の規則は [systemd の認証情報名](credentials.ja.md#github-app-と追加の認証情報)
と同じです。既存の安全でない権限やシンボリックリンクは、自動修正せず拒否します。

デーモンの起動・再起動・設定反映は自動では行いません。
`ghalet config validate` の後に、起動または
[`ghalet apply`](operations.ja.md#設定反映)を明示的に実行してください。

## 交換とロールバック

PAT は旧トークンの期限前に新しいものを作成します。App の鍵は期限なしで利用でき、
トークンの更新ではなく、ポリシーや漏えいの疑いに応じて交換します。
[認証情報のローテーション](authentication.ja.md#認証情報のローテーション)も参照してください。

計画的な交換では、名前・方式・App の識別情報を変更しません。

```sh
ghalet credential set default --prompt --no-default
ghalet apply --config /absolute/path/to/config.yaml
```

旧値は `NAME.pat.previous` または `NAME.pem.previous` に `0600` で保持します。
バックアップを処理するまで次の交換は拒否します。影響する全 target を確認してから、
旧認証情報を失効させ、バックアップを削除してください。
新しい値で失敗した場合は、旧認証情報が有効な間にバックアップを表示された保存先へ戻し、
再度 apply します。登録時のファイル更新に失敗した場合は以前のファイルへの復元を試み、
復元自体の失敗も明示します。

所有者限定の権限では、root や同じ OS ユーザーで動くジョブから秘密を守れません。
[信頼できるホストの境界](operations.ja.md#信頼境界)を維持し、必要に応じて
ジョブの実行ユーザーを分離してください。バックアップにも平文のシークレットを含みます。
