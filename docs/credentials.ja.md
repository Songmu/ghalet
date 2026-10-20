# systemd 用の暗号化した認証情報

[English](credentials.md) | [日本語](credentials.ja.md)

`ghalet systemd credential set` は、パッケージの `ghalet.service` 用に名前付きの
PAT または GitHub App の秘密鍵を登録します。Linux、root 権限、systemd 250 以降、
`systemd-creds`、`systemctl` が必要です。`--prompt` には
`systemd-ask-password` と対話端末も必要です。パッケージのインストール・更新中に
認証情報を要求せず、このコマンドもサービスを起動・再起動しません。

## PAT の登録

先に[パッケージの設定](distribution.ja.md#debian--rpm-パッケージ)をコピーして、
target の scope などを編集してから PAT を登録します。

```sh
sudo ghalet systemd credential set default --prompt
```

デフォルトの方式は `pat` です。入力方法は一つだけ明示してください。
未指定時に対話入力へ切り替えることはありません。

```sh
sudo ghalet systemd credential set default --file /secure/pat-file
sudo ghalet systemd credential set default --stdin < /secure/pat-file
```

`--file` には所有者だけが読める通常ファイル（通常は `0600`）を指定します。
入力ファイルは削除・変更しません。シークレットをコマンド引数、シェル履歴、
YAML の値、環境変数には書かないでください。入力キャンセル、空入力、無効な鍵、
暗号化失敗では既存の登録を変更しません。入力の上限は 1 MiB です。

登録時にはメモリ上の値を直接 `systemd-creds` に渡し、
`--name=default --with-key=host` で暗号化します。
`/etc/ghalet/credentials/default.cred` に権限 `0600` で書くのは暗号文だけです。
ホスト鍵は `/var/lib/systemd/credential.secret` にあります。暗号文とともに保護し、
バックアップ方法も決めてください。この鍵にアクセスできる root から秘密を守る
仕組みではなく、元のホスト鍵なしに別ホストで復号することはできません。

専用の drop-in を作成し、`systemctl daemon-reload` を実行します。

```ini
# /etc/systemd/system/ghalet.service.d/credential-default.conf
[Service]
LoadCredentialEncrypted=default:/etc/ghalet/credentials/default.cred
```

さらに `/etc/ghalet/config.yaml` の `defaults.credential` を置き換えます。
他の設定やコメントは保持しますが、YAML の書式は正規化される場合があります。
別の設定は `--config PATH` で指定できます。ただし unit は引き続き
`/etc/ghalet/config.yaml` を使い、`ExecStart` は変更しません。
target に明示された認証設定も書き換えません。

```yaml
defaults:
  credential:
    type: pat
    tokenFile: /run/credentials/ghalet.service/default

targets:
  - id: project
    scope: OWNER/REPO
```

`credential` を省略した target だけが共通設定を継承し、`credential: {}` は `gh` を
選びます。上書きの規則は[認証情報の継承](operations.ja.md#設定と認証情報)を参照してください。

検証と起動は明示的に実行します。

```sh
sudo ghalet config validate --config /etc/ghalet/config.yaml
sudo systemctl enable --now ghalet
sudo journalctl -u ghalet -f
```

設定の検証では GitHub の認証・権限は確認しません。systemd はサービス起動時に、
アクセス制限された読み取り専用の実行時ディレクトリへ復号した認証情報を渡します。
サービス外で直接起動したプロセスには、そのファイルが自動で渡るわけではありません。

## GitHub App と追加の認証情報

暗号化されていない RSA PEM 秘密鍵（PKCS#1 または PKCS#8）を一つ取り込みます。

```sh
sudo ghalet systemd credential set app-work-key --type github-app \
  --client-id APP_CLIENT_ID --file /secure/app.pem
```

共通設定を `type: github-app`、`clientId`、
`privateKeyFile: /run/credentials/ghalet.service/app-work-key` に置き換えます。
`--client-id` を省略した場合、既存の App 共通設定の client ID を再利用します。
この設定を使う各 target には `installationId` が必要で、自動推測はしません。
installation token は既存の SDK が取得するため、手入力は不要です。

YAML を変更せずに追加登録するには `--no-default` を指定します。

```sh
sudo ghalet systemd credential set secondary --prompt --no-default
```

利用する target に `credential.type: pat` と
`credential.tokenFile: /run/credentials/ghalet.service/secondary` を指定します。
App キーでは代わりに `type: github-app`、`clientId`、`privateKeyFile` と、
target の `installationId` を指定します。同じパスを複数 target で共有したり、
別名を参照したりできます。名前は先頭を英数字とする 1〜64 文字の ASCII 英数字・
アンダースコア・ハイフンです。

## ローテーションとロールバック

旧 PAT を失効させる前に新 PAT を発行するか、旧 App キーを削除する前に追加の鍵を
生成します。ローテーションでは名前・認証方式・App の識別情報を変えず、
`--no-default` で設定を変更せずに新しい値を登録します。

```sh
sudo ghalet systemd credential set default --prompt --no-default
sudo systemctl stop ghalet
# デーモンの終了結果とログで正常終了を確認した場合だけ続けます。
sudo systemctl start ghalet
```

App キーの場合は、`--prompt` の代わりに
`--type github-app --file /secure/new.pem` を使います。
暗号文はアトミックに置き換え、旧暗号文は
`/etc/ghalet/credentials/NAME.cred.previous` に保持します。
バックアップを処理するまで次の更新は拒否します。コマンドが管理する drop-in に
想定外の変更があれば上書きしません。ファイル更新や daemon reload に失敗した場合は
以前のファイルへの復元を試み、復元自体の失敗も明示します。ファイルの復元に失敗した
場合は旧暗号文のバックアップを保持し、エラーにそのパスを表示します。

`apply` で再読込する通常の認証情報ファイルと異なり、systemd の暗号化認証情報は
サービス起動時にのみ更新します。上の手順どおり通常停止と正常終了の確認を行い、
[強制終了や不完全な drain](operations.ja.md#容量再試行停止)は解消してから起動してください。
同じ認証情報を共有する全 target が一緒に切り替わります。
対象 target 全てで認証と runner の動作を確認したら、旧 PAT を失効させるか旧 App
キーを削除し、ロールバック用の暗号文も削除します。

```sh
sudo rm -- /etc/ghalet/credentials/default.cred.previous
```

新しい認証情報で失敗した場合は、旧認証情報を有効なまま保ちます。通常停止と
正常終了の確認後に暗号化バックアップを戻し、再び起動します。

```sh
sudo systemctl stop ghalet
# デーモンの終了結果とログで正常終了を確認した場合だけ続けます。
sudo mv -- /etc/ghalet/credentials/default.cred.previous /etc/ghalet/credentials/default.cred
sudo systemctl start ghalet
```

ローテーションのロールバックでは YAML は変えません。認証方式や App の識別情報を
変更する場合は、対応する設定も別途保持・復元してください。PAT の有効期限、
Organization の承認、GitHub App 秘密鍵のローテーションは引き続き管理者が管理します。
