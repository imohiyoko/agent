# ADR 0004: MCP は namespace ごとの gateway を通し、使える server と tool を namespace で切り替える

- 状態: Proposed
- 日付: 2026-11-23
- きっかけ(人の依頼、2026-11-23): namespace ごとに MCP の権限を切り替える方法を考えて実装する。

## 要旨

- agent に MCP server を直接起動させない。`nsctl` が namespace ごとの **MCP gateway** を持ち、agent はそこにだけ繋ぐ。
- gateway は、宣言に書いた server だけを見せ、その server の中でも許した tool だけを見せる。許していない tool の呼び出しは、server に渡す前に断る。
- MCP server が要る認証情報は gateway の側で渡す。agent は値を見ない。
- MCP server 自身も、その namespace のサンドボックスと proxy の中で動かす(server の通信も namespace の規則に従う)。

## 決定

### 1. 宣言

```yaml
mcp:
  servers:
    - name: files
      command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/data"]
      env:
        SOME_TOKEN: {secret: research/some-token}
      tools: [read_file, list_directory]   # 許す tool。書いていないものは見せない
      allow_resources: false               # resources/* を許すか(既定は拒否)
      allow_prompts: false                 # prompts/* を許すか(既定は拒否)
```

- `tools` に `*` は書けない。全部を許すときも、名前を全部書く(server の更新で新しい tool が勝手に増えないように)。
- `secret` は自分の namespace のもの(`<namespace>/...`)だけ参照できる。

### 2. 繋ぎ方

- gateway は MCP の Streamable HTTP を話す。agent からは `http://127.0.0.1:3129/mcp/<server 名>` に見える。入口の分け方は ADR 0002 の proxy と同じ(Linux は namespace ごとの unix socket、macOS は namespace ごとの port)。
- 実行のたびに、MCP の設定ファイル(Claude Code の `--mcp-config` の形)を作り、agent のコマンドの `{mcp_config}` に入れる。
- gateway は MCP のセッション(`Mcp-Session-Id`)ごとに server のプロセスを 1 つ起動し(stdio)、セッションの終わり(`DELETE`)か、一定時間使われなかったときに止める。

### 3. 検査

| JSON-RPC のメソッド | 扱い |
|---|---|
| `initialize`、`ping`、`notifications/*` | 通す |
| `tools/list` | 通し、返ってきた一覧から許していない tool を消す |
| `tools/call` | `params.name` が許した tool のときだけ通す。違えば server に渡さずエラー(`-32601`)を返す |
| `resources/*`、`prompts/*` | 宣言で許したときだけ通す |
| それ以外 | 断る |

- server から agent への要求(sampling など)は、第 1 段では通さない(記録して捨てる)。
- 判定はすべて namespace の監査ログに残す。

## 限界

- 第 1 段で扱う server は stdio のものだけ。リモートの HTTP の MCP server は、gateway から認証情報を付けて中継する形で後で足す。
- server から agent への通知の中継(SSE の `GET`)は第 1 段では無い(`405` を返す)。
