# ADR 0009: agent が何を叩いたかを nsctl が実行ごとに記録し、見張り役の agent に読ませる

- 状態: Proposed
- 日付: 2026-11-25
- きっかけ(人の言葉、2026-11-25)
  - 「自由に叩いて欲しいけど、何を叩くのかを監視する agent を用意することができないっていうのが気残りなんだよ」
  - 見張りの形の提案に「お、それはいいね」

## 要旨

- **困りごと**: agent にはサンドボックスの中で bash などを自由に使わせたい。ただ、何を動かしたかを見張る agent を置く道が無い。
- **採らない形**: agent のコマンドを orchestrator 側の仮の端末に通して、そこで見る形。
  - orchestrator が狙われやすい部品になり、そこが止まると全部止まる。
  - bash を本当に禁じることはできない(agent は別の言語や別の道具で同じことができる)。
- **決定(案)**
  1. nsctl が、実行ごとに「何をしたか」を出来事として記録する。中身は次の 3 つ。
     - プロセスの起動(exec): 何を、どの引数で
     - 通信の宛先: proxy が通した・止めたもの
     - MCP の呼び出し: gateway が通した・止めたもの
  2. 置き場所は ADR 0008 の出来事と同じ(`GET /v1/namespaces/{ns}/runs/{id}/events`)。種類で分ける。
  3. 見張り役の agent は、自分の namespace で動かす。できることは 3 つだけにする。
     - 見張る先の出来事を読む
     - 実行を止める
     - 人に知らせる
  4. 見張りは基本、後から読む(非同期)。前もって止める(実行の前に見張り役か人の許可を待つ)のは、危ない操作だけにする。
  5. orchestrator は、agent から来た文字列をコマンドとして実行しない。仕事は `run.create` で頼む。

## 決定

### 1. 記録するもの

| 種類 | どこで取るか | 中身 | 今あるか |
|---|---|---|---|
| `exec` | サンドボックスの中の `nsctl sandbox-init`(下の 2) | 時刻、プログラムのパス、引数、親子関係 | 無い(足す) |
| `net` | proxy | 宛先、メソッドとパス(認証情報の経路のとき)、通した・止めた | 監査ログにある。実行の id が無い |
| `mcp` | gateway | server、tool、通した・止めた | 監査ログにある。実行の id が無い |
| `report` / `ask` | gateway(ADR 0008) | agent からの報告と質問 | 無い(ADR 0008 で足す) |

- `net` と `mcp` は、ADR 0007 の決定 2(実行ごとの入口)で実行の id が分かるようになる。そのあとで、出来事にも書く。
- 中身に認証情報の値は書かない(design.md の性質 4)。proxy が付けた値は記録の前に除く。引数に秘密が入ることは防げない(限界を見る)。
- 大きさに上限を付ける。1 件の大きさ(引数は切り詰める)、1 実行の件数。越えたら「記録をやめた」という出来事を 1 件書く。記録をやめたことは隠さない。

### 2. exec の取り方

| OS | 方法 | 強さ |
|---|---|---|
| Linux | `sandbox-init` が、agent を起動する前に seccomp の filter(`execve`、`execveat` に `SECCOMP_RET_USER_NOTIF`)を自分に掛ける。通知を受ける fd を nsctl に渡し、nsctl が起動のたびに `/proc/<pid>/cmdline` と exe を読んで記録する | 子と孫のすべての起動を取れる。filter は外せない(seccomp の filter は掛けたら外せない)。`sandbox-init` は掛ける前に自分で no_new_privs を立てる |
| macOS | `nsctl serve` が、agent のプロセスの木を短い間隔で見て、新しいプロセスを記録する | 弱い。すぐ終わるプロセスは見落とす |

- Linux の別案: `ptrace`(`PTRACE_O_TRACEEXEC`)。agent 自身が `ptrace` を使う道具(gdb、strace)を使えなくなるので、第 1 候補にしない。
- Linux の別案: `fanotify` の `FAN_OPEN_EXEC`。特権が要るので使わない。
- macOS の別案: Endpoint Security。Apple の許可(entitlement)と root が要るので、今は使わない。

### 3. 前もって止める(危ない操作だけ)

- 前もって止めるのは、nsctl が中身を確かに知っている所だけにする。
  - `net`: 宣言で「許可を待つ」と書いた宛先(例: `api.github.com` への `POST`)。proxy が止めて待つ。
  - `mcp`: 宣言で「許可を待つ」と書いた tool(例: `create_pull_request`)。gateway が止めて待つ。
- 待っているあいだ、出来事に「許可待ち」を書く。見張り役の agent か人が、API で許すか断る(`POST .../events/{n}/answer`、ADR 0008 と同じ形)。
- 時間内に答えが無ければ断る(既定は拒否)。
- `exec` は、前もって止める対象にしない(第 1 段)。
  - seccomp の通知で引数を読んでから許すまでのあいだに、agent が引数を書き換えられる(seccomp_unotify(2) に、これを安全の判断に使うなと書いてある)。
  - bash を止めても、agent は別の道で同じことができる。危ない結果は、通信と MCP で止めるほうが確か。
- 宣言の書き方(案):

```yaml
network:
  allow:
    - host: api.github.com
      methods: [GET]
    - host: api.github.com
      methods: [POST]
      approve: true      # 許可を待つ
mcp:
  servers:
    - name: github
      tools: [get_file_contents, create_pull_request]
      approve: [create_pull_request]
monitor:
  namespace: watch       # 見張り役の namespace
```

### 4. 見張り役の agent

- 見張り役は、自分の namespace(例 `watch`)で動く、ふつうの agent。
- 見張り役ができること(nsctl の gateway が見せる nsctl 自身の tool):

| tool | 中身 | 範囲 |
|---|---|---|
| `events_read` | 見張る先の実行の出来事を読む | 見張る先の宣言の `monitor.namespace` に自分の namespace が書いてある実行だけ |
| `run_stop` | 実行を止める | 同上 |
| `approve` | 許可待ちに答える | 同上 |
| `notify_human` | 人に知らせる(出来事に「人の判断が要る」を書く) | 同上 |

- 見張り役は、見張る先の通信も、鍵も、ファイルも見ない。出来事だけを読む。
- 見張り役に、nsctl の API のトークンは渡さない。tool は gateway の中で nsctl が答える。
- 見張り役自身の出来事も記録する。見張り役を見張るのは人(監査ログ)。
- 見張り役が止まっている・遅れているときも、agent は止めない(非同期の見張りなので)。許可待ちは時間切れで断られる。

### 5. orchestrator の決まり

- orchestrator は、agent から来た文字列(`report` の中身、ログ、出力)をコマンドとして実行しない。シェルにも渡さない。
- 仕事を頼むときは `run.create` だけを使う。動くのは宣言に書いた agent のコマンドだけ(ADR 0003)。
- agent の出力を次の依頼の文に入れるのはよい(依頼文は agent への入力で、コマンドではない)。

## 確認の状態(2026-11-25)

- **確認済み**(ソース)
  - `sandbox-init` は agent を起動する前に動く(`internal/sandbox/init.go`)。filter を掛ける場所はある。
  - bwrap は `--unshare-pid` などで動かしている(`internal/sandbox/sandbox.go`)。
  - proxy と gateway は、判定を監査ログに残している(ADR 0002、0004)。
- **確認済み**(文書): seccomp のユーザー通知は Linux 5.0 から。filter を掛けるのに特権は要らず、no_new_privs があればよい。通知で読んだ引数は、許すまでに書き換えられうるので、安全の判断に使うなと書いてある(seccomp(2)、seccomp_unotify(2) の man page。GitHub の mkerrisk/man-pages の master を 2026-11-25 に読んだ。seccomp_unotify(2) の NOTES に「this mechanism must not be used to make security policy decisions」「time-of-check, time-of-use race」とある)。
- **未確認**
  - bwrap の中で `sandbox-init` が filter を掛け、通知の fd を外の nsctl に渡せること(試していない)。
  - 起動を記録するときの重さ(ビルドのように起動が多い仕事で、どのくらい遅くなるか)。
  - macOS でプロセスの木を見る間隔と、見落としの割合。
- **推測**: ふつうの coding agent の仕事では、起動の数は 1 実行で数千まで。上限は 1 万件から始めて、使ってみて直す。

## 限界

- macOS では、すぐ終わるプロセスを見落とす。Linux と同じ強さにはならない。
- 引数に秘密(トークンなど)が書かれると、そのまま記録に入る。記録は nsctl の状態の置き場所に置き、`run.read` の権限で読む。見張り役もそれを読む。
- 前もって止めるのは、通信と MCP だけ。サンドボックスの中の操作(ファイルの削除など)は、後から知るだけ。被害は、書き込める場所(宣言)の中に限られる。
- 見張り役の agent が判断を誤ることは防げない。止める・許すの記録は残る。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| agent のコマンドを orchestrator 側の仮の端末に通す | 全部のコマンドを前もって見られる | orchestrator が狙われやすく、止まると全部止まる。bash を本当には禁じられない |
| すべての exec を前もって止める | 一番厳しい | 遅い。引数の書き換えで抜けられる(TOCTOU)。見張りが止まると仕事が止まる |
| 記録だけで、見張り役を置かない(人が後で読む) | 単純 | 人が常に見ているとは限らない。気づくのが遅い |
| agent の中の仕組み(Claude Code の hooks など)で記録する | 作るのが楽 | agent の協力に頼る。agent が別の道で動かせば記録されない |

## 人が決めること

1. この形(記録は nsctl、見張り役は別の namespace、前もって止めるのは通信と MCP の危ない操作だけ)でよいか。
2. macOS の exec の記録が弱いこと(見落としがある)を受け入れるか。

## 関連

- ADR 0002(proxy)、0003(API と権限)、0004(gateway)、0007(実行ごとの入口と主体)、0008(出来事の置き場所)
