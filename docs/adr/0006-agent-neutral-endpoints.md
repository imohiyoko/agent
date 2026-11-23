# ADR 0006: どの coding agent でも同じ境界で動かすため、経路と gateway の URL を置き換えと環境変数で渡す

- 状態: Accepted(2026-11-23、imohiyoko の委任により agent が決定)
- 日付: 2026-11-23
- きっかけ(人の問い、2026-11-23): 「claude 以外のコーディング agent にも対応できるようになってる?」

## 要旨

- **困りごと**
  - 境界は agent に依らないが、agent の側の設定の渡し方が Claude Code に寄っていた。
  - `{mcp_config}` は Claude Code の形だけ。
  - 認証情報の経路(`http://<name>.creds.internal`)は、agent が `HTTP_PROXY` に従うことを前提にしている。
  - Gemini CLI は、loopback 以外への http を拒否する。
- **決定**
  1. proxy は、直接呼ばれたとき `/creds/<name>/<path>` でも経路を受ける。`http://<name>.creds.internal/<path>` と同じ処理で、規則・gateway だけの経路・監査も同じ。
  2. 置き換え `{creds_url}`(例 `http://127.0.0.1:3128/creds`)と `{mcp_url}`(例 `http://127.0.0.1:3129/mcp`)を足す。`command`・`interactive`・`env` の値で使える。
  3. 環境変数 `NSCTL_CREDS_URL` をいつも渡す。`NSCTL_MCP_URL` は、その namespace に MCP の server があるときに渡す。
  4. `NO_PROXY=127.0.0.1,localhost` にする。loopback(proxy と gateway)には proxy を通さずに繋ぐ。
  5. `nsctl doctor` は、env の値が指す経路が宣言に無いとき、または https で書いたときに警告する。
- agent ごとの設定の書き方は [docs/agents.md](../agents.md) に置く。agent ごとの特別な処理は nsctl に入れない。

## 理由

- 置き換えと環境変数だけなら、新しい agent が出ても nsctl のコードは変えずに済む。宣言と文書で対応できる。
- `/creds/` の口は、agent ができることを広げない。どの経路を使えるかも、経路の規則も、これまでと同じ。

## 限界(この決定で守れないこと)

- **agent の中でログインしたトークン**: 守れない。Codex CLI(ChatGPT)や Gemini CLI(Google アカウント)をサンドボックスの中でログインして使うと、トークンは agent の HOME に置かれ、agent が読める。proxy に持たせられるのは、長く使える鍵やトークンだけ。
- **agent が設定を無視すること**: 防げない。ただし、agent が宛先や鍵の設定を無視しても、届く先は変わらない。
  - 通信は proxy と gateway しか通らず、規則も同じ。
  - agent の手元に本物の鍵は無い。
- **実際の agent で動くこと**: Claude Code のほかは確かめていない(下の「確認の状態」)。
- **ファイルに書き写した URL**: macOS では port が namespace ごと・起動ごとに変わりうる。`settings.json` や `opencode.json` に書き写した URL は古くなる。置き換えか環境変数を使う。

## `NO_PROXY` を loopback にしてよい理由

サンドボックスの中から loopback で届くのは、その namespace の proxy と gateway だけ。

- Linux: ネットワークの namespace が別で、中には `nsctl sandbox-init` の 2 つの port しか無い。
- macOS: Seatbelt が、その namespace の 2 つの port のほかを拒否する。

これまでは `NO_PROXY` を空にしていた。そのため、`HTTP_PROXY` に従い、loopback を自分で除かない agent は、gateway への通信も proxy に送ってしまう。proxy は loopback への通信を拒否するので、その agent からは MCP が使えなかったはず(どの agent が当てはまるかは確かめていない)。

## 確認の状態(2026-11-23)

- **確認済み**(テスト): 次の 2 つ。
  - `/creds/<name>/` の経路が、`.creds.internal` と同じ規則で動くこと。次のものは拒否される。
    - 規則に無いパス
    - `..` や `%2e%2e`
    - 無い経路
    - gateway だけの経路
  - bwrap の中で、`{creds_url}` と `{mcp_url}` で `HTTP_PROXY` を設定したまま上流と gateway に届くこと。
- **未確認**: Codex CLI、Gemini CLI、Aider、OpenCode を実際に動かすこと。この環境では動かす許可が無かった。設定は公式の文書とソースから書いた([docs/agents.md](../agents.md) に出典)。
