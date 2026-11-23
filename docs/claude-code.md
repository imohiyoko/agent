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
