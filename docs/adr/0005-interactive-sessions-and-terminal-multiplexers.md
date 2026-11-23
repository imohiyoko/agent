# ADR 0005: 人の端末で agent を対話で動かす(nsctl shell)。端末の道具(cmux など)とは組み合わせて使う

- 状態: Accepted(2026-11-23、imohiyoko の委任により agent が決定)
- 日付: 2026-11-23
- きっかけ(人の依頼、2026-11-23): 「cmax みたいなの」を作りたい(cmax は cmux の打ち間違いと推測)。内部では Max プランで Claude Code を対話で使うことが多い。

## 要旨

- **困りごと**: これまでの実行は `nsctl run`(1 つのプロンプトを渡し、出力をログに残す)だけだった。Claude Code を人がふだん使う形(端末で対話)では、namespace の中で動かせなかった。
- **決定**: `nsctl shell <namespace> [agent]` を足す。宣言の `agents[].interactive` のコマンドを、その namespace のサンドボックスの中で、**呼んだ人の端末にそのまま繋いで**動かす。
  - 通信、認証情報、MCP の tool は、`nsctl run` とまったく同じ規則に従う。
- **cmux との関係**: cmux は、AI の coding agent を何個も並べて使うための macOS の端末。サンドボックス、namespace ごとの認証、通信の制限は持たない(確認済み: cmux.com の Claude Code の頁と claude-teams の記事、2026-11-23)。nsctl は cmux を作り直さない。cmux のペインやワークスペースで `nsctl shell` を開けば、見た目と通知は cmux、境界は nsctl になる。

## 決定

### 1. 端末は nsctl の中を通さない

`nsctl shell` は、同じ機械の `nsctl serve` に、状態の置き場所の中の control socket(`private/control.sock`、`0600`)で繋ぐ。

- 最初の通知で、自分の端末の stdin・stdout・stderr の 3 つのファイル記述子を渡す(`SCM_RIGHTS`)。
- `nsctl serve` は、そのファイル記述子を agent にそのまま与えて起動する。
- agent は人の端末を直接読み書きする。nsctl は何も中継しない。
  - 端末の機能(raw モード、色、マウス、ベル)がそのまま使える。
  - 疑似端末を作り直したり画面を写したりするコードが要らない。

| 選択肢 | 採らなかった理由 |
|---|---|
| API(HTTP / WebSocket)で端末の入出力を中継する | 疑似端末の扱い、画面の大きさ、制御文字の中継を自分で作る必要がある。遠くの機械から使う必要は今はない |
| `nsctl shell` が自分でサンドボックスを作る | proxy と gateway は `nsctl serve` が持っている。2 つのプロセスで規則を持つと、ずれる |

### 2. だれが使えるか

control socket は状態の置き場所(`0700`)の中にあるので、`nsctl` を動かしている OS の利用者だけが繋げる。その利用者はトークンと秘密のファイルをもともと読めるので、admin と同じに扱う。監査ログの主体は `local` になる。

API のトークンを持つだけの人や仕組みは使えない(API には対話の口を作らない)。

### 3. 対話の実行の決まり

| | `nsctl run`(prompt) | `nsctl shell`(interactive) |
|---|---|---|
| コマンド | `agents[].command`(`{prompt}` を使う) | `agents[].interactive`(`{prompt}` は使えない) |
| 出力 | ログに残す(`max_log_bytes` まで) | 残さない(人の端末にだけ出る) |
| 時間の上限 | `timeout` / `default_run_timeout` | 無し。agent が終わる、端末を閉じる、`nsctl stop` で終わる |
| 実行の一覧 | `mode: prompt` | `mode: interactive` |
| 同時に動かせる数 | どちらも `max_runs_per_namespace` に数える | 同左 |

- 端末(cmux のペインなど)を閉じて `nsctl shell` が消えると、`nsctl serve` がその実行を止める。
- 窓の大きさが変わったことを agent に伝える。
  - macOS: `nsctl shell` が受けた `SIGWINCH` を伝え、`nsctl serve` が agent に送る。
  - Linux: bwrap がシグナルを中に渡さないので、中の `nsctl sandbox-init` が端末の大きさを 1 秒に 4 回見て、変わったら agent に `SIGWINCH` を送る。
- `nsctl shell` 自身は Ctrl-C・Ctrl-\・Ctrl-Z で終わらない。キーは agent が読む(Claude Code は端末を raw モードにして自分で扱う)。
- Linux では bwrap の `--new-session` により、agent は端末に文字を押し込めない(`TIOCSTI` を使えない)。

## 確認の状態(2026-11-23)

- **確認済み**(Linux、bwrap、Claude Code 2.1.330)
  - `nsctl shell` で対話の Claude Code が起動した。
  - 打ったプロンプトが `claude.creds.internal/v1/messages` を通り、proxy が付けた値が Anthropic に届いた(偽のトークンなので `401 Invalid bearer token`)。
  - 自動更新(`downloads.claude.ai`)と `github.com` への CONNECT は `network.allow` に無いので拒否された。
  - 窓の大きさと `TERM` が中に伝わった。Ctrl-C 2 回で終了した。
  - 試験(`TestShell`)は次のことを見る。
    - 入出力が繋がること
    - 終了コード
    - 出力を記録しないこと
    - 対話のコマンドが無い agent を断ること
    - 端末側が消えたら止まること
- **未確認**
  - macOS(`sandbox-exec`)での対話は、CI のビルドと vet だけ。
  - cmux の中で動かすこと(cmux は macOS のアプリで、この環境では動かせない)。
- **既知の制約**: Linux のサンドボックスの中では `/dev` を作り直すので、`tty` コマンドは端末の名前を正しく出さない(読み書きには影響しない)。
