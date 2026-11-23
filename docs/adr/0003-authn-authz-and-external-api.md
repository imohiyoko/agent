# ADR 0003: 認証は「人と仕組み」だけに持たせ、namespace ごとの役割で認可し、外からの指示は API で受ける

- 状態: Proposed
- 日付: 2026-11-23
- きっかけ(人の依頼、2026-11-23): 認証認可の方法、権限を制御する方法、外部 API 経由で色々指示を出せるようにする方法を考えて実装する。

## 要旨

- **agent は認証を持たない**。agent は API に届かない(ADR 0002 のネットワークの境界)。agent の権限は、入れられた namespace の宣言だけで決まる。
- **API を呼ぶ主体**(人、CI、ほかの仕組み)はトークンで認証する。トークンは `nsctl token create` で作り、値は作ったときに 1 回だけ表示する。保存するのは SHA-256 のハッシュだけ。
- **認可**は「主体 × namespace × 操作」。namespace の宣言の `members` に主体と役割を書く。書いていない namespace には何もできない(見えもしない)。
- 外からの指示は HTTP の API(`/v1/...`)で受ける。指示で動かせるのは、宣言に書いた agent だけ。任意のコマンドは受けない。

## 決定

### 1. 主体と認証

- 主体の名前は `^[a-z][a-z0-9-]{0,62}$`。
- トークンは 32 バイトの乱数を `nsk_` に続けて base64url にしたもの。状態の置き場所の `tokens.json`(`0600`)に、ハッシュ・主体・作った日時・名前(用途のメモ)だけを持つ。
- 検査は、受け取った値のハッシュで探す。比べるのはハッシュ同士なので、値の長さや中身で時間が変わらない。
- トークンの取り消しは `nsctl token revoke <id>`。
- トークンには期限がある(既定 90 日、`-ttl` で変える。`-ttl 0` で期限なし)。期限切れは認証に失敗する。
- API は既定で `127.0.0.1` だけで受ける。外の機械から使うときは、TLS を終端する手前の仕組み(リバースプロキシ、Tailscale など)を置く前提にする。`0.0.0.0` で受けるときは起動の引数で明示させ、警告を出す。
- 後で足すもの: OIDC、mTLS。トークンと同じく「主体の名前」を返す部品として足せる形にする。

### 2. 役割と操作

| 操作 | viewer | operator | admin |
|---|---|---|---|
| `namespace.read`(宣言の要約を見る) | ○ | ○ | ○ |
| `run.read`(実行の一覧、状態、ログ) | ○ | ○ | ○ |
| `run.create`(宣言の agent に指示を出す) | | ○ | ○ |
| `run.stop` | | ○ | ○ |
| `audit.read`(通信と API の監査ログ) | | | ○ |

- 役割は namespace ごと。同じ主体が、namespace A では operator、B では viewer になれる。
- 設定ファイルの `admins` に書いた主体は、全 namespace で admin。
- 判定は 1 つの関数(`authz.Allowed`)だけで行う。API の各入口はこの関数を通す。
- 宣言(namespace、メンバー、通信の規則)を API で変える操作は作らない。変える経路は PR だけ(design.md の性質 6)。

### 3. API(v1)

| メソッドとパス | 操作 |
|---|---|
| `GET /v1/whoami` | 認証できれば誰でも |
| `GET /v1/namespaces` | 自分に `namespace.read` がある namespace だけ返す |
| `GET /v1/namespaces/{ns}` | `namespace.read` |
| `GET /v1/namespaces/{ns}/runs` | `run.read` |
| `POST /v1/namespaces/{ns}/runs` | `run.create`。本文は `{"agent": "<名前>", "prompt": "<文>"}` |
| `GET /v1/namespaces/{ns}/runs/{id}` | `run.read` |
| `GET /v1/namespaces/{ns}/runs/{id}/logs` | `run.read` |
| `POST /v1/namespaces/{ns}/runs/{id}/stop` | `run.stop` |
| `GET /v1/namespaces/{ns}/audit` | `audit.read` |

- 権限が無いときは、namespace の存在を漏らさないよう `404` を返す(`whoami` と一覧を除く)。
- `prompt` は agent のコマンドの `{prompt}` の位置に 1 つの引数として入れる。シェルは通さない。
- API の呼び出しはすべて監査ログに残す(主体、操作、namespace、結果)。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| agent にもトークンを持たせ、agent ごとに認可する(herdrOrchestrator の ADR 0007 の形) | 細かく決められる | agent が持つ秘密は漏れうる。人の依頼の趣旨(agent に認証を付けない)と違う |
| API で宣言も変えられるようにする | 運用が楽 | 宣言を変える経路が 2 つになり、レビューを通らない変更ができる |
| 任意のコマンドを API で受ける | 柔軟 | API のトークンが漏れたときの被害が大きい |
