# Claude Code を namespace の中で動かす

`nsctl` の namespace の中で Claude Code(`claude -p`)を動かす手順と、確かめた結果。

## 宣言

```yaml
credentials:
  - name: anthropic
    upstream: https://api.anthropic.com
    header: x-api-key
    secret: <namespace>/anthropic
    rules:
      - methods: [POST]
        paths: ["/v1/messages", "/v1/messages/count_tokens"]
filesystem:
  writable:
    - ~/work/<namespace>
    - ~/agents/<namespace>   # この namespace の agent の HOME(~/.claude などはここに作られる)
agents:
  - name: claude
    command: ["claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--strict-mcp-config"]
    workdir: ~/work/<namespace>
    home: ~/agents/<namespace>
    timeout: 30m
    env:
      ANTHROPIC_BASE_URL: http://anthropic.creds.internal
      ANTHROPIC_API_KEY: placeholder-replaced-by-proxy
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
```

- `ANTHROPIC_BASE_URL` を認証情報の経路にすると、Claude Code の API の通信は proxy を通り、proxy が本物の API キーを `x-api-key` に入れる。agent の環境にあるのは置き換え用の値だけ。
- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` で、API 以外の通信(Claude Code 自身の補助的な通信)を止める。付けないと `api.anthropic.com:443` への CONNECT が試みられ、`network.allow` に無いので拒否される(動作には影響しなかった)。
- `--strict-mcp-config` で、`nsctl` が作った MCP の設定(その namespace の gateway)だけを使わせる。
- `home` で namespace ごとに HOME を分ける。Claude Code の設定と記録(`~/.claude`、`~/.claude.json`)が namespace ごとに別になり、ほかの namespace の会話の記録は読めない。`home` は `filesystem.writable` の中に置く。

## 対話で使う(nsctl shell)

ふだんの端末で Claude Code を使うのと同じように、namespace の中で対話で動かせる。通信・認証情報・MCP の規則は `nsctl run` と同じ。決定の理由は [ADR 0005](adr/0005-interactive-sessions-and-terminal-multiplexers.md)。

### 宣言

agent に `interactive` を足す。`{prompt}` は使えない(人が打つ)。

```yaml
agents:
  - name: claude
    command: ["claude", "-p", "{prompt}"]   # nsctl run 用
    interactive: ["claude"]                 # nsctl shell 用
    env:
      DISABLE_AUTOUPDATER: "1"              # 自動更新(downloads.claude.ai)を止める
```

MCP を使う namespace では、`interactive: ["claude", "--mcp-config", "{mcp_config}", "--strict-mcp-config"]` とする。

### 始める

`nsctl serve` を動かしている機械で、**同じ OS の利用者**が、同じ設定で打つ。

```sh
nsctl shell personal            # agent が 1 つだけなら名前を省ける
nsctl shell research claude
```

- 監査ログの主体は `local` になる。`nsctl runs <namespace>` では `mode: interactive` と出る。
- 出力はログに残らない。時間の上限も無い。
- 端末を閉じると、`nsctl serve` がその実行を止める。ほかの端末から `nsctl stop <namespace> <run-id>` でも止められる。

### 最初の 1 回だけ

その namespace の `home` で初めて Claude Code を開くと、初回の案内が出る。案内の中の接続の確かめは `api.anthropic.com` に直接繋ごうとし、`network.allow` に無いので失敗する。案内を済んだことにしておく。

```sh
H=~/agents/personal   # agent の home
[ -e "$H/.claude.json" ] || echo '{"hasCompletedOnboarding": true}' > "$H/.claude.json"
```

その後、作業場所(`workdir`)を信頼するかを 1 回だけ聞かれる。「Yes, I trust this folder」を選ぶ。

### 確かめたこと(2026-11-23、Linux、bwrap、Claude Code 2.1.330)

| 確かめたこと | 結果 |
|---|---|
| 対話の画面が出て、キーで操作できる | できた(疑似端末から操作) |
| 打ったプロンプトの通信 | `credential POST claude.creds.internal/v1/messages allowed`。偽のトークンなので `401 Invalid bearer token`(proxy が付けた値が届いた) |
| ほかの宛先 | `downloads.claude.ai:443`(自動更新)と `github.com:443` への CONNECT は拒否された。`DISABLE_AUTOUPDATER=1` で前者は止まる |
| 窓の大きさ・`TERM` | 中に伝わった |
| 終わり方 | Ctrl-C 2 回で Claude Code が終わり、`nsctl shell` も終わった |

macOS での対話は未確認。

## Max / Pro プランで動かす(API キーなし)

Claude の Max / Pro プランは API キーではなく OAuth のトークンで認証する。nsctl では、このトークンも API キーと同じく **proxy だけが持ち、agent には渡さない**。

### 手順

1. **トークンを作る**(サンドボックスの外、ふだんの端末で)。

   ```sh
   claude setup-token
   ```

   ブラウザでログインすると、長く使えるトークン(`sk-ant-oat01-…`)が表示される。

2. **nsctl の秘密のファイルに入れる**。値は画面にも履歴にも残らない。

   ```sh
   nsctl secret set personal/claude-oauth
   # value for personal/claude-oauth (not shown): ← ここに貼り付けて Enter
   ```

   入れた名前は `nsctl secret list` で見られる(値は出ない)。`nsctl serve` を止める必要はない(次の通信から使われる)。

3. **宣言を書く**。全体は [examples/namespaces/personal.yaml](../examples/namespaces/personal.yaml)。

   ```yaml
   credentials:
     - name: claude
       upstream: https://api.anthropic.com
       header: Authorization
       format: "Bearer {value}"
       secret: personal/claude-oauth
       rules:
         - methods: [POST]
           paths: ["/v1/messages", "/v1/messages/count_tokens"]
   agents:
     - name: claude
       command: ["claude", "-p", "{prompt}"]
       env:
         ANTHROPIC_BASE_URL: http://claude.creds.internal
         CLAUDE_CODE_OAUTH_TOKEN: placeholder-replaced-by-proxy
         CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
   ```

4. **確かめる**: `nsctl doctor` が秘密と設定を見る。

### API キーのときとの違い

| | API キー | Max / Pro プラン |
|---|---|---|
| 秘密の中身 | API キー(`sk-ant-api…`) | `claude setup-token` のトークン(`sk-ant-oat01-…`) |
| 経路の `header` | `x-api-key` | `Authorization`(`format: "Bearer {value}"`) |
| agent の環境変数 | `ANTHROPIC_API_KEY: <置き換え用>` | `CLAUDE_CODE_OAUTH_TOKEN: <置き換え用>` |

- 両方の環境変数を置くと、Claude Code は API キーを使う。`nsctl doctor` が警告する。
- 宣言に本物のキーやトークン(`sk-ant-` などで始まる値)を書くと、`nsctl validate` が拒否する。宣言は git に入るため。

### サンドボックスの中で `claude /login` しない理由

中でログインすると、トークンは agent の HOME(`~/.claude/.credentials.json`)に置かれる。agent が自分でトークンを読めて、外へ持ち出せる。トークンの更新にも、ログイン用のサイトへの通信を許す必要がある。macOS では、トークンは Keychain に入るので、サンドボックスからは使えない。`setup-token` のトークンを proxy に持たせれば、この 3 つがどれも起きない。

### 確かめたこと(2026-11-23、Claude Code 2.1.330)

本物のトークンでは確かめていない(人の判断で行わない)。代わりに、`ANTHROPIC_BASE_URL` を手元の HTTP サーバーに向け、Claude Code が何を送るかを見た。

| 確かめたこと | 結果 |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` を置くと、`ANTHROPIC_BASE_URL` に送るか | 送った(`POST /v1/messages?beta=true`) |
| トークンの載せ方 | `Authorization: Bearer <トークン>`。`anthropic-beta` に `oauth-2025-04-20` が付く |
| 置き換え用の値(`placeholder-replaced-by-proxy`)でも送るか | 送った(形を手元で確かめていない) |
| ほかの宛先への通信(`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` のとき) | 無かった |

proxy は経路の `header` を**置き換える**(`Header.Set`)ので、agent が付けた置き換え用の値は上流に届かない。

## 確かめた結果(2026-11-23、Ubuntu 24.04、bwrap、Claude Code の CLI)

確かめたときは `home` を使わず、`~/.claude` と `~/.claude.json` を書き込み可にしていた(`home` はその後に足した)。秘密のファイルには偽の API キーを入れ、`nsctl serve` の API から `nsctl run <ns> claude "say hi"` で動かした。

| 確かめたこと | 結果 |
|---|---|
| Claude Code がサンドボックスの中で起動する | 起動した |
| `/v1/messages` が認証情報の経路を通る | 監査ログに `credential POST anthropic.creds.internal/v1/messages allowed` |
| proxy が付けた値が API に届く | API の答えが `Invalid API key`(偽のキーなので想定どおり。値が届いたことを示す) |
| 許していない宛先への通信 | `CONNECT api.anthropic.com:443` が `not in network.allow` で拒否された(上の環境変数を付けると試みも無くなった) |
| 実行の結果 | `failed`(終了コード 1、API キーが偽のため) |
| `kill -HUP` での宣言の読み直し | 実行中の `nsctl` で `updated [cc]` |

本物の API キーでの確認は、人の手元で行う(`nsctl doctor` で秘密が揃っていることを見てから `nsctl run`)。
