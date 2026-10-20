# 認証のセットアップと最小権限

[English](authentication.md) | [日本語](authentication.ja.md)

GitHub.com のリポジトリ・Organization の target 用に認証情報を発行する手順です。
常駐サービスには GitHub App、小規模な構成には fine-grained personal access token
（PAT）を推奨します。`type: gh` は GitHub CLI のトークンを再利用する方式であり、
追加の権限を付与するものではありません。その他の GitHub ホストでは、そのホストの
API ドキュメントとポリシーを確認してください。

## 認証方式の選び方

| 方式 | メリット | 注意点 |
| --- | --- | --- |
| GitHub CLI（`gh`、デフォルト） | 既存のログインを使い、ghalet 専用のシークレットファイルなしで試せる | 既存ログインの権限と有効性を共有する。デーモンの環境で `gh` とログイン・キーチェーンを利用できる必要があり、OAuth ログインはリポジトリ単位に限定できない |
| Fine-grained PAT（`pat`） | リポジトリと権限を選べ、ファイル参照で簡単に設定できる | 個人と一つの resource owner にひも付く。有効期限、再発行、必要に応じた Organization の承認を管理する必要がある |
| GitHub App（`github-app`） | 個人のログインではなくインストールの識別情報で動作し、リポジトリと権限を選べる。短命な installation token は SDK が更新する | 初期登録、インストール承認、ID・鍵の管理が増える。長期利用する秘密鍵自体の保護とローテーションは必要 |

共有・長期運用には専用の App、単純な専用構成には fine-grained PAT、
既存の対話環境で試す場合には `gh` を使います。
GitHub の [PAT ガイド][pat-guide]、[App の概要][app-overview]、
[installation 認証ガイド][app-installation]も参照してください。
どの方式でも、ジョブの OS ユーザーが読める認証情報からジョブを隔離するものではありません。
[信頼できるホストの境界](operations.ja.md#信頼境界)を守ってください。

## 最小権限

workflow の保存先ではなく、target の `scope` に合わせて権限を選びます。
リポジトリと Organization の target では必要権限が異なります。

| 認証情報 | リポジトリ scope（`OWNER/REPO`） | Organization scope（`ORG`） |
| --- | --- | --- |
| Fine-grained PAT | リポジトリの **Administration: Read and write**。アクセス先を対象リポジトリに限定 | Organization の **Self-hosted runners: Read and write**。Resource owner を対象 Organization に設定 |
| GitHub App | リポジトリの **Administration: Read and write**。対象リポジトリにインストール | Organization の **Self-hosted runners: Read and write**。対象 Organization にインストール |
| GitHub CLI の OAuth トークン | `repo` | `admin:org`。REST API のドキュメントでは非公開リポジトリに `repo` も要求 |

自動で付くリポジトリの **Metadata: Read-only** はそのままにします。
Organization の target だけに使う App には、リポジトリの Administration は不要です。
リポジトリと Organization の両方を扱う場合は両方の必要権限を付けるか、
認証情報を分けて権限を分離してください。

この最小権限は、[固定バージョンの Scale Set SDK][sdk-auth] が利用する
[リポジトリ][repo-registration]・[Organization][org-registration] の
登録トークン発行 API に基づきます。SDK はそのトークンを Actions サービス用の
認証情報に交換し、Scale Set、runner group、runner を管理します。
ghalet の管理用認証情報に Contents、Workflows、Actions の権限は不要です。
ジョブの `GITHUB_TOKEN` や workflow の `permissions` は別のものであり、
そちらの権限を増やしても ghalet の認証は通りません。
有効期間の短い runner 登録トークンを直接指定する認証方式には対応していません。

PAT は発行者の権限を超えて操作できません。リポジトリの target にはそのリポジトリの
管理者、Organization の target には Organization の owner を使ってください。
Organization のポリシーによってトークンや App の利用制限・承認が必要な場合があります。
リポジトリの Administration や `repo` は runner 専用ではない広い権限です。
Fine-grained PAT や App のインストール範囲で、影響するリポジトリを限定してください。
Organization の runner 権限は Organization 全体に及び、リポジトリの選択では
制限できません。runner group のリポジトリ利用許可は GitHub 側で別途設定します。
ghalet は既存 group を参照し、そのポリシーは変更しません。

## Fine-grained PAT の作成

[GitHub の PAT 作成ガイド][pat-guide]に沿って作成します。

1. 個人の **Settings → Developer settings → Personal access tokens →
   Fine-grained tokens → Generate new token** を開きます。
2. 用途の分かる名前と有効期限を設定し、**Resource owner** にリポジトリの所有者または
   対象 Organization を選びます。Fine-grained PAT 一つで扱える resource owner は
   一つだけなので、異なる所有者には別のトークンを用意します。
3. リポジトリの target では **Only select repositories** で対象だけを選び、
   リポジトリの **Administration: Read and write** を付けます。
   Organization の target では Organization の **Self-hosted runners:
   Read and write** を付けます。Organization 単位の runner のためだけに
   リポジトリの権限を追加する必要はありません。
4. その他の権限は付けずにトークンを生成します。Organization の承認が必要な場合は、
   承認後に利用してください。
5. 表示された値を安全に保存します。Linux パッケージのサービスでは、
   [PAT の暗号化登録](credentials.ja.md#pat-の登録)を使ってください。

Linux / macOS で直接起動する場合は、デーモンの実行ユーザーが読める、
所有者のみアクセス可能な通常ファイルに PAT を保存し、値を YAML に書かずに参照します。

```sh
chmod 600 /absolute/path/to/ghalet.pat
```

```yaml
defaults:
  credential:
    type: pat
    tokenFile: /absolute/path/to/ghalet.pat

targets:
  - id: project
    scope: OWNER/REPO
```

Classic PAT も `type: pat` で利用できますが、新規設定では
fine-grained PAT または GitHub App を推奨します。

`tokenEnv` は `tokenFile` の代替であり、併記はできません。パッケージのサービスでは
環境変数によるシークレット保存を避けてください。トークンの値をコマンド引数、
シェル履歴、コミットするファイル、runner の環境設定に書かないでください。

## GitHub CLI

`type: gh` ではデーモンの実行ユーザーでログインします。
既存のログインが GitHub CLI の OAuth トークンを使っている場合は、
必要な scope を要求します。

```sh
# Repository targets.
gh auth refresh --hostname github.com --scopes repo
# Organization targets; include repo as well when required for private repositories.
gh auth refresh --hostname github.com --scopes admin:org
```

これは既存の CLI ログインに scope を追加する操作であり、専用の最小権限トークンを
作るものではありません。既に他の scope が付いている場合もあります。
GitHub CLI に PAT を渡している場合は、その PAT の権限を変更するか再発行してください。
OAuth の scope 更新で PAT の権限は増えません。
[GitHub CLI の scope 更新リファレンス][gh-refresh]と、キーチェーンやサービス環境についての
[サービスユーザーの認証](operations.ja.md#設定と認証情報)を参照してください。

## GitHub App の作成とインストール

[GitHub の App 登録ガイド][app-guide]に沿って作成します。

1. App を所有するアカウントの **Settings → Developer settings → GitHub Apps →
   New GitHub App** を開きます。Organization の runner には、
   Organization 所有の App を使ってください。
2. 一意の名前と **Homepage URL**（このプロジェクトのリポジトリ URL など）を設定します。
   Callback URL と Setup URL は空欄にし、ユーザー認可や device flow は有効にしません。
   ghalet は installation 認証を使います。
3. **Webhook** の **Active** を外します。Webhook URL、公開リスナー、
   イベントの購読設定は不要です。
4. 上の表から対象 scope に必要な権限だけを設定します。それ以外は、自動で付く Metadata の
   読み取り権限を除いて **No access** のままにします。所有アカウント内だけで使う App では、
   インストール可能な範囲に **Only on this account** を選び、App を作成します。
5. App の設定ページにある **Client ID** を控え、ghalet の `clientId` に使います。
   Client secret や installation ID とは別の値です。Client secret は不要です。
   **Credentials → Key pairs → New key**（旧設定画面では
   **Private keys → Generate a private key**）で秘密鍵を生成し、
   ダウンロードした PEM ファイルを安全に保存します。
6. **Install App** から、target を所有するアカウントにインストールします。
   リポジトリの target では **Only select repositories** で対象だけを選びます。
   Organization の target はその Organization へのインストールが必要です。
   リポジトリの選択で Organization 全体の runner 管理権限は制限できません。
7. インストールの設定ページを開き、URL 末尾の数値を **installation ID** として控えます。
   例は `https://github.com/organizations/ORG/settings/installations/12345678` または
   `https://github.com/settings/installations/12345678` です。
   このインストールを使う各 target に `installationId: 12345678` を設定します。

直接起動する場合は、ダウンロードした秘密鍵を所有者のみ読み取り可能にし
（`chmod 600 /absolute/path/to/ghalet-app.pem`）、次のように設定します。

```yaml
defaults:
  credential:
    type: github-app
    clientId: Iv1.REPLACE_WITH_CLIENT_ID
    privateKeyFile: /absolute/path/to/ghalet-app.pem

targets:
  - id: project
    scope: OWNER/REPO
    installationId: 12345678
```

Organization の target では `scope` を `ORG` に変更し、その Organization の
installation ID を使います。複数のインストールで App の client ID と鍵を共有できますが、
各 target には所有者に対応する installation ID が必要です。
Installation token の取得・更新は ghalet の SDK が自動で行います。
パッケージのサービスでは、鍵を設定ディレクトリへ直接保存する代わりに、
[App キーの取り込み](credentials.ja.md#github-app-と追加の認証情報)を使ってください。
秘密鍵はその App のどのインストールとしても認証できるため、厳重に保護してください。
[GitHub の秘密鍵管理ガイド][app-keys]と
[認証情報のローテーション](credentials.ja.md#ローテーションとロールバック)も参照してください。

## 認証情報のローテーション

Installation token の自動更新と、秘密鍵のローテーションは別です。
SDK が Actions サービス用の認証情報を更新しても、PAT の有効期限が延びたり、
失効した PAT が置き換わったりはしません。PAT は期限前に再発行し、
App の鍵は運用ポリシーに従ってローテーションしてください。

パッケージのサービスで暗号化した認証情報を使う場合は、
[systemd のローテーションとロールバック](credentials.ja.md#ローテーションとロールバック)に従い、
同じ名前に `--no-default` で新しい値を登録してから、通常停止・正常終了の確認・起動を行います。
`apply` では systemd が復号した認証ファイルは更新されません。

Linux / macOS で通常の PAT ファイルや App キーファイルを使う場合は、次の手順で更新します。

1. 同じ最小権限・リポジトリ範囲の **新しい** PAT を作るか、既存 App に秘密鍵を
   **追加生成**します。必要な承認・SSO の設定も先に済ませます。
   計画的な切り替えの間は旧認証情報を有効なまま保持し、先に再生成・失効させないでください。
2. 旧ファイルをアクセス制限した状態でロールバック用に保持し、設定先のファイルを
   新しい値でアトミックに置き換えます。所有者のみアクセス可能な権限と、
   デーモンの実行ユーザーによる読み取りを維持します。
   App の鍵を更新しても `clientId` と target の `installationId` は変えません。
3. `ghalet apply --config /absolute/path/to/config.yaml` を実行します。
   全 target を drain し、パスが同じでも新しいクライアントがファイルを再読込します。
   [drain の期限](operations.ja.md#容量再試行停止)を考慮し、
   無停止・無中断の更新が保証されるとは考えないでください。
4. Apply の結果、`ghalet doctor`、デーモンのログ、信頼できるテストジョブで、
   **その認証情報を共有する全 target** を確認します。成功後に初めて GitHub 側で
   旧 PAT を失効させるか旧 App キーを削除し、不要になったロールバック用のコピーも削除します。

新しい認証情報で失敗した場合は、失効操作の前に保護した旧ファイルを戻し、
再度 `apply` します。認証方式や App の識別情報も変更する場合は、
対応する YAML も保持・復元してください。Apply の失敗で設定が自動ロールバックするわけではありません。

`tokenEnv` では、シェルの変数を書き換えても稼働中のデーモンには反映されません。
起動環境を更新し、通常停止と正常終了の確認後に起動してください。
`gh` では、デーモンの実行ユーザーでログインを差し替え・再認可した後に `apply` し、
新しいクライアントに現在のトークンを取得させます。
[設定反映](operations.ja.md#設定反映)も参照してください。
認証情報が漏えいした場合は、通常の移行期間を設けず直ちに失効させ、
安全な新しい認証情報でサービスを復旧してください。

## 設定の確認

同じ設定・実行ユーザーで `ghalet config validate`、`ghalet run`、
`ghalet doctor` の順に確認します。設定の検証はローカルで行うもので、
GitHub の権限確認ではありません。デーモンが対象の Scale Set を登録し、
その名前を `runs-on` に使う信頼できるテスト workflow を実行できることを確認してください。

認証や登録に失敗した場合は、トークンの期限切れ・失効、PAT 発行者のアクセス権、
resource owner と承認、App の権限とインストール時の承認、
対象リポジトリ、client ID・installation ID・鍵の組み合わせを確認します。
Runner group でも workflow のリポジトリが許可されている必要があります。
アクセスエラーを解消するために全権限を付けることは避けてください。

[repo-registration]: https://docs.github.com/ja/rest/actions/self-hosted-runners#create-a-registration-token-for-a-repository
[org-registration]: https://docs.github.com/ja/rest/actions/self-hosted-runners#create-a-registration-token-for-an-organization
[sdk-auth]: https://github.com/actions/scaleset/blob/e6daac702355cdb5b880b4fbdcf6d85dcd9e48e5/client.go
[pat-guide]: https://docs.github.com/ja/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens
[app-guide]: https://docs.github.com/ja/apps/creating-github-apps/registering-a-github-app/registering-a-github-app
[app-keys]: https://docs.github.com/ja/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps
[app-overview]: https://docs.github.com/ja/apps/creating-github-apps/about-creating-github-apps/about-creating-github-apps
[app-installation]: https://docs.github.com/ja/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation
[gh-refresh]: https://cli.github.com/manual/gh_auth_refresh
