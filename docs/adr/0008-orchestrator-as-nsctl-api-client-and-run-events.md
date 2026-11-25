# ADR 0008: orchestrator は nsctl の API の客にし、実行の途中の報告は nsctl が実行ごとに溜める

- 状態: Proposed(人の選択を待つ)
- 日付: 2026-11-25
- 置き換えるもの: [ADR 0007](0007-split-boundary-and-orchestrator-roles.md) の決定 3(agent から tb の API への道)と、段階 4 の「tb の API」
- きっかけ(人の言葉、2026-11-25): 「tb からの API が必ずしも良いとは言えない。あれは結局 herdr に妥協した構成だから」

## 要旨

- **困りごと**: ADR 0007 の決定 3 は、agent がサンドボックスの中から tb(herdrOrchestrator の taskboard)の API を呼ぶ形だった。
  - tb は herdr の pane 越しに conductor と worker がやりとりする前提で作られた。その形をそのまま残すことになる。
  - サンドボックスから外への出口(tb への経路)が 1 つ増える。
  - orchestrator が tb に縛られる。
- **決定(案)**
  1. orchestrator は nsctl の API の客にする。依頼は `run.create`、結果は実行の状態・ログ・出力で受ける。agent から orchestrator に繋ぐ道は作らない。
  2. 実行の途中の報告と質問のために、nsctl の gateway に nsctl 自身の tool(`report`、`ask`)を置く。届いたものは、その実行の「出来事」(event)として追記だけで溜める。
  3. orchestrator は出来事を API で読む(`GET /v1/namespaces/{ns}/runs/{id}/events`)。`ask` への答えは API で返す(`POST .../events/{n}/answer`)。
  4. nsctl は溜めて渡すだけ。宛先の振り分け、次の仕事の判断、承認はしない。それは orchestrator の仕事。

## 決定

### 1. 依頼と結果(今ある API)

| すること | API | 今あるか |
|---|---|---|
| 依頼する | `POST /v1/namespaces/{ns}/runs`(`agent`、`prompt`、`profile`) | ある(`profile` は ADR 0007 で足す) |
| 状態を見る | `GET .../runs/{id}` | ある |
| 出力を読む | `GET .../runs/{id}/logs` | ある |
| 止める | `POST .../runs/{id}/stop` | ある |

- 仕事の単位は実行 1 つ。orchestrator は、実行の id を自分の仕事の記録に結び付ける。

### 2. 途中の出来事(足す)

| tool(gateway が見せる) | 中身 | orchestrator 側 |
|---|---|---|
| `report` | `{"text": "..."}`。進み具合 | `GET .../events` で読む |
| `ask` | `{"question": "...", "choices": [...]}`。決めてほしいこと | 読んで、`POST .../events/{n}/answer` で答える。agent の `ask` は答えが来るまで待つ(上限の時間あり) |

- tool は gateway の中で nsctl が答える。外の MCP server も、外への経路も使わない。
- 出来事の主体は、実行ごとの入口で決まる(ADR 0007 の決定 2)。agent は別の実行の名前で書けない。
- 宣言で使うかを決める(`mcp.builtin: [report, ask]`。プロファイルでも選べる)。既定は使わない。
- 大きさに上限を付ける(1 件の大きさ、1 実行の件数)。越えたら断る。
- 出来事は実行が終わっても、ログと同じ期間だけ残す。
- API の権限: 読むのは `run.read`、答えるのは `run.create` と同じ operator 以上(ADR 0003 の表に足す)。
- 監査ログに、書いた・答えたを残す。

### 3. orchestrator に要るもの

- nsctl の API のトークン(割り振り先の namespace の operator)。ADR 0007 の決定 4 と同じ。
- 出来事を待つ仕組み。第 1 段は API を間隔を空けて読む。あとで、変化を待つ API(長く待つ `GET` か SSE)を足してよい。
- conductor のような LLM の orchestrator を使うなら、それも 1 つの namespace で動かす。nsctl の API を使う tool は、その namespace の gateway に置く(認証情報の経路で nsctl の API に繋ぐ)。nsctl の API のトークンは conductor には見えない。

## 確認の状態(2026-11-25)

- **確認済み**(ソース): 依頼・状態・ログ・停止の API はある(`internal/server/api.go`、ADR 0003)。
- **未確認**: `ask` で agent を待たせたとき、各 agent(Claude Code など)の MCP の呼び出しの時間切れがどうなるか。長く待てないなら、`ask` はすぐ返し、答えは次の `report` か別の tool で受け取る形にする。
- **推測**: 途中の報告が要らない仕事(1 回の依頼で終わる仕事)は、出来事の tool を使わずに済む。

## 限界

- agent が `report` に秘密を書くことは防げない。届くのは、その実行が読めたものに限られる。
- nsctl に保存するものが増える(出来事)。nsctl の状態の置き場所に置き、ログと同じ扱いにする。
- 実行どうしの直接のやりとりは無い。A の報告を B に渡すのは orchestrator の仕事。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| agent が tb の API を呼ぶ(ADR 0007 の決定 3) | 今の tb を使える | herdr の頃の形を残す。サンドボックスからの出口が増える。orchestrator が tb に縛られる |
| nsctl の API だけ(途中の出来事を作らない) | nsctl が一番小さい | 途中の質問ができない。長い仕事の進み具合が見えない |
| GitHub の issue や PR を仕事の置き場所にする | 履歴が残る。人も同じ所で見られる | 遅い。外のサービスに依りきりになる。agent に書き込みの tool を渡す必要がある |
| nsctl に仕事の管理(タスク、割り振り)まで入れる | 1 つで済む | 境界の部品の役割が広がる(ADR 0007 で採らなかった) |

## 人が決めること

1. この形(nsctl の API + 途中の出来事)でよいか。
