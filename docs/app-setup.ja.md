# GitHub App のセットアップウィザード

[English](app-setup.md) | [日本語](app-setup.ja.md)

`ghalet auth setup NAME` は [GitHub の Manifest フロー][manifest]で、
個人または Organization が所有する非公開の GitHub App を作成します。
事前の PAT、client secret、公開コールバックサービス、PEM のコピー＆ペーストは不要です。
Linux / macOS で **GitHub.com の設定済み target 一つにつき一つの App** を扱います。
保存先は[暗号化されていない通常ファイル](file-credentials.ja.md)です。
デーモンの起動や設定反映は行いません。

## ローカルでのセットアップ

認証情報はまだ追加せず、利用する scope の設定を作成します。

```yaml
targets:
  - id: project
    scope: OWNER/REPO
```

Organization の target では `OWNER/REPO` の代わりに `ORG` を使います。
デーモンと同じユーザー・StateDir の環境で実行してください。

```sh
ghalet auth setup project-app --target project
```

別の設定ファイルには `--config PATH` を使います。
デフォルトは `~/.config/ghalet/config.yaml` です。
target が一つだけなら `--target` は省略できます。複数 target・所有者には、
別の認証情報名で個別に実行します。既存 App の権限を広げる処理は行いません。

1. 端末で target、App の所有者、必要な書き込み権限を確認します。
   リポジトリでは `Administration`、Organization では `Self-hosted runners` です。
2. ローカルのブラウザ画面で **Continue to GitHub** を選びます。
   意図したアカウントでログインし、提案された App 名を確認して作成してください。
   Organization のポリシーやアカウントの権限によっては owner の操作が必要です。
3. GitHub がローカルのコールバックへ戻ります。CLI は一時コードを交換して鍵と
   公開の App 情報を保存し、インストール画面を開きます。
4. 対象アカウントにインストールします。リポジトリの target では
   **Only select repositories** でそのリポジトリだけを選び、必要な承認を待ちます。
5. CLI はインストールを検出し、App・アカウント・必要な書き込み権限を検証して、
   target の設定変更を表示します。`y` で保存するか、辞退して後から再開します。
   `--yes` が省略するのは最後の設定保存の確認だけで、GitHub 側の承認は省略しません。

他の target と `defaults.credential` は変更しません。選択した target に
`type: github-app`、`clientId`、`privateKeyFile`、`installationId` を設定します。
ユーザーの OAuth 認可や Webhook イベントは要求せず、Webhook の配信は無効です。
作成した App は所有アカウント内だけで利用できます。

一時的な loopback サーバーは Manifest の交換完了後または失敗時に終了します。
コールバックのランダムな `state` と Host を検証し、鍵をブラウザの画面・URL・
コマンド出力には含めません。インストールの検出には App JWT を使い、
ユーザートークンやブラウザから渡された installation ID は使いません。

保存した設定を確認してから実行します。

```sh
ghalet config validate
ghalet run
```

既存デーモンには別のデーモンを起動せず、
[`ghalet apply`](operations.ja.md#設定反映) を明示的に実行してください。
セットアップ成功はインストールと権限の確認であり、実際の runner・ジョブの実行確認ではありません。

## SSH とブラウザの手動操作

ブラウザから CLI の loopback コールバックに到達できる必要があります。
SSH ホストでは、ブラウザのあるマシンから固定ポートを転送します。

```sh
ssh -L 8765:127.0.0.1:8765 runner-host
# In that SSH session:
ghalet auth setup project-app --target project \
  --listen 127.0.0.1:8765 --no-browser
```

SSH 接続を維持したまま、表示された URL をローカルのブラウザで開いてください。
`--no-browser` だけではリモートの localhost に到達できません。
待受先は loopback IP アドレスに限り、`0.0.0.0` や公開インターフェースは使えません。
同じホストで手動操作する場合は、SSH 転送なしで `--no-browser` を使えます。
公開のコールバックや Webhook エンドポイントは不要です。

## 再開と認証情報の保存

鍵は `<StateDir>/credentials/NAME.pem` に保存し、`NAME.app.json` に公開の App 識別情報・
scope・鍵のパスを保持して再開できるようにします。
両方とも `0600` で、認証ディレクトリは `0700` です。
JSON に PEM、client secret、webhook secret は保存しません。
PEM とバックアップは[通常ファイルの認証情報](file-credentials.ja.md)の説明どおり保護してください。

一回の待機時間は最大 15 分です。中断、設定保存の辞退、インストールの承認後は、
同じコマンド・設定・target・名前で再実行します。
App 情報の保存後は既存の App と鍵を再利用し、App の再作成や鍵の交換は行いません。
scope・所有者・鍵のパスの不一致、App 情報のない既存の鍵、同名のセットアップの同時実行、
設定ファイルの同時編集は拒否します。GitHub App を自動削除することはありません。

GitHub で App 作成後、認証情報の保存前にコールバックや交換が失敗した場合は、
やり直す前に App の設定を確認してください。Manifest の交換は GitHub の制限である
1 時間以内に完了する必要があります。鍵の保存後に App 情報を書けなかった場合は、
エラーに保持した鍵のパスを表示します。むやみに App を再作成せず、
既存 App の設定と認証情報の手動取り込みを使ってください。

設定ファイルは実行ユーザーが所有し、他のユーザーが書き込めないものだけを更新します。
コメントは保持しますが、YAML の書式やエイリアスが正規化される場合があります。
インストール権限の検証で runner group のアクセス設定は変更しません。

パッケージの systemd サービスでは、生成した PEM を
[`ghalet systemd credential set`](credentials.ja.md#github-app-と追加の認証情報) で取り込みます。
選択した target の明示的な `credential.privateKeyFile` を表示された systemd の
実行時パスに変更するか、明示的な credential を削除して新しい共通設定を継承させ、
`installationId` は保持します。共通設定だけを変更しても、ウィザードが明示した target の
認証設定には優先しません。サービスの設定を検証してから起動し、
暗号化登録の動作確認と復旧方法の確保後にだけ平文のコピーを削除してください。
ブラウザでのセットアップと、権限が必要なサービスへの登録は別の操作です。

App の秘密鍵は有効期限がなく、installation token は自動更新します。
鍵はポリシーや漏えいの疑いに応じて交換します。
[認証情報とローテーション](authentication.ja.md#認証情報のローテーション)も参照してください。
この機能で、同じユーザーのジョブから認証情報を隔離することはできません。

[manifest]: https://docs.github.com/ja/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
