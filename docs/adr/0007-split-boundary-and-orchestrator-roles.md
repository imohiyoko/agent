# ADR 0007: 境界(認証・通信・認証情報・起動)は nsctl、仕事の割り振りは orchestrator、画面は herdr に分ける

- 状態: Accepted(2026-11-25。役割の分け方は人の提案。「人が決めること」の 4 つに人が答えた。最後は MCP のプロファイルの形に「それでいいと思う」)
- 日付: 2026-11-25
- きっかけ(人の言葉、2026-11-25)
  - 「ハーダーでは agent 認証とネットワーク分離がうまくいってなかった。こっちではどこまでできる?」
  - 「ハーダーの役割とおーけすとれーたーの役割で分離するべきな気がしてきた」
  - 「ハーダー」は herdr(herdrdev/herdr)のこと(人の訂正、2026-11-25)。
  - 「herdr にその機能はないと思うから新規に作る必要はありそう」「terminal 単位、workspace 単位で分離したいよね」(2026-11-25)

## 要旨

- **困りごと**: herdrOrchestrator は、仕事の割り振り(tb、conductor、Web UI、scheduler)と、agent の認証・制限を 1 つのアプリでやろうとした。
  - 認証は「呼び出し元がどの pane の中か」で決めていた。agent と人と tb が同じ OS ユーザーなので、わざとのなりすましは止めきれない(herdrOrchestrator ADR 0007 の段 1 で「一部許容」とした)。
  - ネットワークの分離(同 ADR 0001 の L4)は手を付けられなかった。herdr には pane を分ける機能が無い。
- **決定**: 3 つの役割に分け、互いの仕事をしない。
  - **nsctl(このリポジトリ)= 境界**: agent の起動、サンドボックス、通信の制限、認証情報、MCP の tool の制限、API を呼ぶ主体の認証と認可。
  - **orchestrator(herdrOrchestrator の tb・conductor・Web UI・scheduler)= 仕事**: タスク、報告、割り振り、承認の流れ、定期の実行。agent を動かすときは nsctl に頼む。自分では agent を起動しない。
  - **herdr = 画面**: 人が agent を見て話す端末。pane の中で `nsctl shell` を動かすだけ。認証には使わない。
- orchestrator は、agent が使う認証情報(GitHub、API の鍵)を持たない。nsctl の API のトークンを、割り振り先の namespace の operator として持つだけにする。

## 決定

### 1. 役割の表

| 仕事 | nsctl | orchestrator | herdr |
|---|---|---|---|
| agent をサンドボックスに入れて起動・停止する | ○ | 頼むだけ(API の `run.create` / `run.stop`) | |
| 通信の宛先の制限 | ○ | | |
| 認証情報(鍵・トークン)を持ち、通信に付ける | ○ | | |
| MCP の server と tool の制限 | ○ | | |
| API を呼ぶ人・仕組みの認証、namespace ごとの役割 | ○ | | |
| 宣言(namespace、通信の規則)を変える | PR だけ(design.md の性質 6) | | |
| タスク、報告、割り振り、承認、定期の実行 | | ○ | |
| 「どの agent に何をさせてよいか」の仕事の上の判定(tb の policy) | | ○(nsctl の境界の内側でだけ効く) | |
| 人が agent を見て話す | | | ○(pane で `nsctl shell`) |

- 仕事の上の判定(tb の policy)は残してよい。ただし「破られても境界は守られる」ものとして扱う。境界は nsctl だけが持つ。
- 「違う権限が要る agent は、別の namespace に入れる」(ADR 0001 の 1)をそのまま使う。tb の役割(manager、worker、reviewer など)ごとに権限を分けたいときは、namespace を分ける。

### 2. agent の主体は「namespace + 実行」にする

人の答え(2026-11-25): 「できたらターミナル単位にしたいが、多分楽なのは namespace 単位」。

- agent は認証を持たない(ADR 0003)。主体は agent の自己申告ではなく、nsctl が付ける。
- 権限の上限は namespace でつく(通信、鍵、MCP の server と tool、HOME、書き込める場所、members)。今の nsctl と同じ。
- terminal(= 1 つの実行)ごとの権限は、上限の中から絞ることでつける(決定 6 の MCP のプロファイル)。
- orchestrator から見た主体は `<namespace>/<実行の id>` にする。gateway が、どの実行の入口から来たかで付ける。agent は変えられない。
  - 決定 6 のために、gateway は実行ごとに入口を分ける。同じ仕組みで実行の id も分かるので、terminal 単位の主体は追加の手間が小さい。
  - 第 1 段は namespace だけでもよい(実行の id が無いときは `<namespace>`)。
- pane、環境変数、プロセスの親子による主体の判定(herdrOrchestrator ADR 0007 の段 1)は、境界としては使わない。
- 限界: 実行の中で agent が子のプロセスや subagent を動かしても、主体は同じ実行のまま。それより細かくは分けない。

### 3. agent から orchestrator への道(報告、タスクの取得)

人の言葉(2026-11-25): 「tb を MCP server にするのはなんかなぁ。tb がプロキシするのが正しそうに思う」。tb は MCP server にしない。

- **報告・タスク**: tb は MCP server ではなく、普通の HTTP の API(`report`、`task_get`、`task_assign` など)を持つ。agent は自分の namespace の認証情報の経路(`{creds_url}/tb/...`)で繋ぐ。
  - 経路に付ける認証情報は「namespace ごとの tb のトークン」。agent はトークンを見ない。
  - 経路の `rules` で、namespace ごとに使える API を絞る(例: worker は `report` と `task_get` だけ、conductor は `task_assign` も)。
  - proxy は上流に `<namespace>/<実行の id>` を付けて送る(決定 2)。
- MCP の許可は tb に聞かない(決定 6。宣言のプロファイルで決まる)。tb は MCP の通信を中継しない。
- サンドボックスの中から tb の DB とソケットは読めないようにする(`filesystem` の隠す場所に入れる)。

### 4. orchestrator から agent への道(割り振り)

- conductor は LLM の agent なので、それ自体も 1 つの namespace(例 `conductor`)で動かす。conductor が nsctl のトークンを持つことはない。
- conductor が「worker にこれをさせる」と決めたら、tb の tool を呼ぶ。tb(決まった処理だけをする部品)が、自分の policy を確かめてから nsctl の API に `run.create` を送る。
- tb が持つ nsctl のトークンは、割り振り先の namespace の `operator` だけにする。admin にはしない。監査ログ(`audit.read`)は admin だけに許される(ADR 0003)。tb には渡さず、人が admin として読む。
- 宣言に書いた agent しか動かせない(ADR 0003)ので、tb が乗っ取られても、任意のコマンドは動かない。

### 5. 分ける単位: herdr の workspace と terminal(pane)

人の言葉「terminal 単位、workspace 単位で分離したい」を、次のように nsctl の単位に当てる。

| herdr の単位 | nsctl の単位 | 分かれるもの |
|---|---|---|
| workspace | namespace | 通信の規則、認証情報、MCP の tool、操作できる人(members) |
| terminal(pane) | 1 つの実行(`nsctl shell` / `nsctl run`) | サンドボックス(プロセス)。実行ごとに別の箱。ただし HOME(`agents[].home`)と書き込める場所(`filesystem.writable`)は宣言で決まり、同じ namespace の実行どうしで共有する。一方の実行が書いたファイルは、もう一方から読める |

- 同じ workspace の中で、pane ごとに違う通信の規則・鍵・HOME・書き込める場所が要るときは、その pane を別の namespace にする。
  - namespace は宣言なので、PR で足す(design.md の性質 6)。
  - 実行中に pane ごとに規則を変える口は作らない。
- herdr の workspace と namespace の対応は、herdr の設定でも nsctl の宣言でもなく、pane の起動コマンド(`nsctl shell <namespace> <agent>`)で決まる。プリセット(`herdr-up.sh`)に書く。

### 6. MCP の接続の許可: 上限も terminal ごとの組み合わせも宣言に書き、起動するときに名前で選ぶ

経緯: 2026-11-25 に imohiyoko が「鍵は nsctl、許可は orchestrator」を選んだ。そのあと「tb に作らなくてもいい」となり、許可も宣言に書く形にした。もとの提案: 「mcp 認証をオーケストレーターが持つようにして、どの agent? terminal? workspace? namespace に対して個別に一つ一つの mcp への接続許可を出す形がいい」。

人の問い(2026-11-25): 「tb に作らなくてもいいけど、どのターミナルや namespace がどの mcp を使えるかってどうやって管理するのがいいのか?」。

「だれが何を使えるか」はすべて namespace の宣言(リポジトリの YAML)に書く。tb は許可を持たない。起動するとき(tb、herdr のプリセット、人)は名前を選ぶだけ。鍵と強制は nsctl に残す。

```yaml
mcp:
  servers:
    - name: github
      credential: github-mcp
      tools: [get_file_contents, list_issues, create_pull_request]   # namespace の上限
  profiles:                                                          # terminal ごとの組み合わせ
    read-only: {github: [get_file_contents, list_issues]}
    pr:        {github: [get_file_contents, create_pull_request]}
agents:
  - name: claude
    mcp_profile: read-only   # 選ばなかったときに使う
```

| 決めること | だれが | どこで | 強制 |
|---|---|---|---|
| その namespace(= workspace)で使えうる MCP の server と tool(上限) | 人 | 宣言の `mcp.servers`。PR で変える | gateway |
| terminal ごとの組み合わせ(プロファイル) | 人 | 宣言の `mcp.profiles`。PR で変える | gateway |
| その実行(= terminal)でどのプロファイルを使うか | 起動する側(tb、herdr のプリセット、人) | `nsctl shell <ns> <agent> --profile <名前>`、`run.create` の `profile`(足す) | gateway |
| MCP の鍵の値 | nsctl だけ | 秘密の置き場所(宣言には名前だけ) | proxy、gateway |

- `nsctl validate` は、プロファイルが上限に無い server・tool を書いていたら断る。
- 宣言に無いプロファイルの名前で起動したら断る(黙って上限にしない)。
- プロファイルを選ばず、`mcp_profile` も無いときは、上限のとおり(今と同じ)。
- 選んだプロファイルは、実行の間は変えられない。
- 組み合わせもリポジトリにあるので、PR でレビューされ、履歴が残る。design.md の性質 6(宣言を変える経路は PR だけ)のまま。
- 弱点: その場で新しい組み合わせを作れない。要るときは PR でプロファイルを足す。
- gateway は実行ごとに入口を分ける(今は namespace ごとに 1 つ。`internal/server/server.go`)。
  - 実行は、入口の URL や鍵ではなく、**どの入口に繋いだか**で見分ける。秘密の値を中に渡さない。同じ namespace の実行は HOME と書き込める場所を共有するので、中に置いた値は別の実行から読めるため。
  - Linux: 入口は実行ごとの unix socket にし、その実行の bwrap にだけ bind する(今の `--bind <socket> <innerDir>/mcp.sock` と同じ形)。socket の実体は nsctl の状態の置き場所にあり、サンドボックスからは隠してある。実行 B のサンドボックスには A の socket が無く、ネットワークの namespace も別なので、B は A の入口に繋げない。
  - macOS: 入口は実行ごとの port にし、その実行の Seatbelt の profile にだけ許す(今は namespace の port を `(allow network-outbound (remote ip "localhost:<port>"))` で許している)。B の profile は A の port を許さないので、B は A の入口に繋げない。
  - どちらも、実行 B から実行 A の入口に繋げないことを試験で示す(段階 3)。
- 「agent」の単位は使わない。agent の名前は agent の自己申告で、境界にならない。agent ごとに分けたいときは、実行ごとにプロファイルを選ぶか、namespace を分ける。
- orchestrator に鍵も許可も持たせない理由:
  - 鍵を持つ部品が乗っ取られると、全部の MCP の鍵がまとめて漏れる。
  - 許可の判定が orchestrator のアプリの中に入ると、herdrOrchestrator でうまくいかなかった「アプリの中の判定」に戻る。
  - orchestrator が乗っ取られても、選べるのは宣言にあるプロファイルだけ。

### 7. 人が見る画面

- herdr の pane では `nsctl shell <namespace> <agent>` を動かす。通信・認証情報・tool の規則は `nsctl run` と同じ(ADR 0005)。
- herdr と cmux の扱いは同じにする。どちらも画面と通知だけを受け持つ。

## 確認の状態(2026-11-25)

- **確認済み**(herdr のソース。herdrdev/herdr の main、0.9.5、commit e8d7d97、2026-11-25 に読んだ)
  - サンドボックス、通信の制限、proxy、認証情報の注入にあたるコードは無い(`src/` と `crates/` を sandbox・landlock・seatbelt・egress・proxy などで検索して 0 件)。
  - pane は herdr の server と同じ OS ユーザーで動く。pane ごとにユーザーや権限を分ける設定は無い。
  - API の socket(`herdr.sock`)は `0600`。同じ OS ユーザーのプロセスなら誰でも繋げる。`SO_PEERCRED` は呼び出し元の pid を知るのに使うだけで、認可には使っていない(`src/api/server.rs`、`src/platform/linux.rs`)。だから pane の中の agent は、API で pane を作り、ほかの agent に打ち込める(README の「agents drive herdr through the cli and socket api」)。
  - 新しい pane には、herdr の server の SSH agent の socket が `SSH_AUTH_SOCK` で渡る(`src/platform/ssh_agent.rs` の `apply_pane_env`)。pane の agent は人の SSH の鍵を使える。
  - plugin は OS ユーザー全体で共通(CHANGELOG 0.7.x の項)。
  - 結論: 人の推測どおり、herdr には agent の認証もネットワークの分離も無い。新しく作る必要がある。その部品が nsctl。
- **確認済み**(このリポジトリのソースと既存の ADR の試験)
  - agent は自分の namespace の proxy と gateway のほかへ通信できない(ADR 0002、0006 の試験)。
  - 認証情報の値は agent に渡らず、proxy が付ける(ADR 0002)。
  - 経路を gateway だけに使わせられ、agent が直接呼ぶと 403 になる(ADR 0004 の 4。`internal/mcpgw/remote.go` の `X-Nsctl-Gateway`)。
  - API は宣言の agent しか動かさず、役割は namespace ごと(ADR 0003)。
  - サンドボックスの環境変数は nsctl が作り直し、nsctl 自身の環境は渡さない。だから herdr の pane から `nsctl shell` を開いても、`SSH_AUTH_SOCK` と `HERDR_*` は中に入らない(`internal/server/server.go` の `baseEnv`)。
  - herdrOrchestrator の認証は pane とプロセスの親子で決めていて、同じ OS ユーザーのなりすましを一部許容している(herdrOrchestrator の docs/adr/0007、2026-11-25 に main を読んだ)。
- **未確認**
  - tb の API は herdrOrchestrator にまだ無い(作る必要がある)。
  - proxy は loopback への通信を拒否する(ADR 0006)。認証情報の経路の上流を、同じ機械の tb(`http://127.0.0.1:<port>` か unix socket)にできるかは確かめていない。できなければ nsctl に「機械の上の上流」を足す変更が要る(ADR を別に書く)。
  - gateway は今、呼んだのが namespace の中のどの実行かを上流に伝えない。決定 2 のため、gateway が実行の id を付ける印を足す(段階 3)。
  - nsctl のサンドボックスの中から herdr の socket(`herdr.sock`)に繋げないこと。Linux の bwrap は `/` を読み取り専用で見せるが、unix socket への接続は読み取り専用でも通るはず(推測)。herdr の状態の場所を `filesystem` の隠す場所に入れる必要がある。macOS の Seatbelt での扱いも確かめていない。
  - SSH agent の socket のファイル自体に、中から繋げないこと。`SSH_AUTH_SOCK` は渡らない(下の確認済み)が、場所を知っていれば繋げるかもしれない。Linux のネットワークの namespace は、ファイルの場所で開く unix socket を分けない。上と同じく隠す場所に入れる。

## 限界(この決定で守れないこと)

- **同じ OS ユーザーの上の orchestrator**: tb と Web UI が nsctl と同じ OS ユーザーで動くなら、tb は nsctl の状態の置き場所(トークン、秘密のファイル)を読める。`nsctl shell` の control socket も admin と同じに扱われる(ADR 0005 の 2)。だから「orchestrator の権限を operator に絞る」は、nsctl を別の OS ユーザーで動かすまでは、取り決めであって強制ではない。
- **conductor の判断の誤り**: conductor が間違った割り振りをしても、nsctl は止めない。止められるのは「宣言の範囲を越えること」だけ。
- **tb の tool を通した持ち出し**: agent が tb の `report` に秘密を書くことは防げない。tb に届くものは、namespace の中で agent が読めたものに限られる。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| herdrOrchestrator の中で認証と分離を作り続ける(ADR 0001 の L3・L4) | 部品が増えない | 同じ OS ユーザー・同じネットワークの上の判定なので迂回できる。herdr に pane を分ける機能が無い |
| nsctl に仕事の管理(タスク、報告)も入れる | 1 つで済む | nsctl の役割が広がり、境界の部品の変更が増える。taskboard を作り直すことになる |
| MCP の鍵と許可の判定を orchestrator に持たせる | 許可を 1 か所で柔軟に出せる | orchestrator が乗っ取られると全部の鍵が漏れる。判定がアプリの中に戻る |
| gateway が呼び出しのたびに tb に許可を聞く | その場で組み合わせを変えられる | 許可がリポジトリの外(tb)に散る。tb が止まると全部止まる |
| MCP の通信そのものを tb に通す | tb が中身を見て細かく決められる | tb に上限の中の鍵の経路を使う権限が要る。乗っ取られると鍵が使える |
| MCP の許可を namespace の宣言の上限だけで決める(今のまま) | 部品が増えない | 実行ごとに絞りたいたびに namespace を足すことになる |
| orchestrator が agent を直接起動し、起動のときだけ nsctl のサンドボックスを借りる | 今の `tb agent-new` を変えずに済む | proxy・gateway と起動が別のプロセスになり、規則がずれる(ADR 0005 で採らなかった形と同じ) |

## 人が決めること

1. ~~agent の主体を namespace の単位にしてよいか。~~ **答え**(2026-11-25、imohiyoko): できれば terminal 単位、楽なのは namespace 単位。→ 決定 2 で、上限は namespace、主体と絞り込みは実行(terminal)単位にした。第 1 段は namespace だけでよい。
2. ~~tb を MCP server として出すか。~~ **答え**(2026-11-25、imohiyoko): MCP server にはしない。tb がプロキシする形がよい。→ tb は普通の API だけを持つ(決定 3)。MCP の許可は宣言のプロファイルで決める(決定 6)。
3. ~~MCP の鍵と許可をどこに置くか(決定 6)。~~ **決まった**: 鍵は nsctl。許可は宣言のプロファイルに書き、起動する側が名前で選ぶ(2026-11-25、imohiyoko の選択と問いから)。
4. ~~nsctl を、人や tb とは別の OS ユーザーで動かすところまでやるか。~~ **答え**(2026-11-25、imohiyoko): やる。orchestrator の権限の絞り込みを強制にするため。
   - 影響: nsctl を別の OS ユーザーにすると、pane の `nsctl shell` は control socket(`0600`、ADR 0005 の 2)に繋げなくなる。段階 6 の前に「pane から別ユーザーの nsctl に繋ぐ方法」を ADR で決める。方法は人が選ぶ。

## 段階(案)

1. この ADR(役割の分け方)。
2. nsctl: 認証情報の経路の上流に、同じ機械の tb を書けるようにする(要るなら)。
3. nsctl: 宣言に `mcp.profiles` と `agents[].mcp_profile` を足し、`run.create` と `nsctl shell` でプロファイルを選べるようにする。gateway が実行ごとに止める。上流(tb)には `<namespace>/<実行の id>` を付けて送る。
4. herdrOrchestrator: tb の API(`report`、`task_get`、`task_assign`)と、nsctl の API でプロファイルを選んで worker を起動する部品。
5. tb が API で起動した worker(`nsctl run`)は対話ではないので、pane では `nsctl logs <namespace> <id> -f`(ADR 0003 のログの API)で出力を見る。人が話しかける pane は、起動コマンドを `nsctl shell` にする(`herdr-up.sh` のプリセット。workspace ごとに namespace を決める)。中から herdr の socket と SSH agent に届かないことを試験で示す。
6. nsctl を別の OS ユーザーで動かす(人が決めること 4)。

## 関連

- ADR 0001(namespace の定義。「herdrOrchestrator との関係」の第 2 段を具体にしたのがこの ADR)
- ADR 0003(認証は人と仕組みだけ。API)
- ADR 0004(MCP gateway、gateway だけの経路)
- ADR 0005(`nsctl shell`、端末の道具との組み合わせ)
- herdrOrchestrator ADR 0001(制限は環境で強制する)、ADR 0007(主体と policy)
