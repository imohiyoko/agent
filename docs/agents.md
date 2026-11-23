# Claude 以外の coding agent を動かす

nsctl の境界(サンドボックス、通信の制限、認証情報の経路、MCP の tool の制限)は、agent の種類に依らない。agent ごとに要るのは次の 3 つだけ。

1. API の宛先を、namespace の認証情報の経路に向ける。
2. API の鍵には置き換え用の値を渡す。
3. MCP を使うなら、gateway の URL を渡す。

例: [examples/namespaces/coding.yaml](../examples/namespaces/coding.yaml)(Codex CLI、Gemini CLI、Aider)。Claude Code は [claude-code.md](claude-code.md)。

agent や MCP server のコマンドは、前もって入れておく。サンドボックスの中からは、`network.allow` に無い npm や PyPI に繋げない。`npx -y <package>` のような実行時の取得は失敗する。

## nsctl が agent に渡すもの

| 置き換え(`command`・`interactive`・`env` の値に書く) | 環境変数(いつも入る) | 中身(Linux の例) |
|---|---|---|
| `{creds_url}` | `NSCTL_CREDS_URL` | `http://127.0.0.1:3128/creds`。経路 `<name>` は `{creds_url}/<name>` |
| `{mcp_url}` | `NSCTL_MCP_URL`(MCP があるとき) | `http://127.0.0.1:3129/mcp`。server `<name>` は `{mcp_url}/<name>`(Streamable HTTP) |
| `{mcp_config}`(`command`・`interactive` だけ) | | Claude Code の `--mcp-config` の形の JSON ファイルの場所 |
| `{prompt}`(`command` だけ、引数 1 つ全体) | | `nsctl run` で渡したプロンプト |

- macOS では port が namespace ごとに違う。だから、URL は書き写さずに置き換えを使う。
- `{creds_url}/<name>` と `http://<name>.creds.internal` は同じ経路で、規則も同じ。
  - `.creds.internal` は、agent が `HTTP_PROXY` を使うときだけ届く。
  - `{creds_url}` は loopback に直接繋ぐので、`HTTP_PROXY` を使わない agent にも届く。
  - Gemini CLI のように、http を loopback でしか許さない agent にも使える。
- `NO_PROXY=127.0.0.1,localhost` が入っている。loopback(proxy と gateway)には直接繋ぐ。サンドボックスの中から loopback で届くのは、その namespace の proxy と gateway だけ。
- MCP の gateway の tool の制限は、どの agent から使っても同じ。

`nsctl doctor` は、agent の env の値を見て、次のときに警告する。

- 指している経路が宣言に無い。調べるのは `{creds_url}/<name>` と `http://<name>.creds.internal` の形だけ。`http://127.0.0.1:3128/creds/<name>` のように書き写した URL は見ない(書き写さずに置き換えを使う)。
- `https://<name>.creds.internal` と書いた。経路は http で送り、TLS は proxy が付ける。

## agent ごとの設定

出典は公式の文書とソース(2026-11 時点)。**この環境では、Claude 以外の agent を実際に動かしての確認はしていない**(動かす許可が無かった)。確かめたのは、nsctl の側(`{creds_url}` の経路、`{mcp_url}`、`NO_PROXY`)をテストで確かめたところまで。使ってみて違ったら、この文書と例を直す。

### OpenAI Codex CLI

| | |
|---|---|
| 1 回の実行 / 対話 | `codex exec "<prompt>"` / `codex` |
| API の宛先 | 設定 `openai_base_url`。環境変数は無いので、`-c openai_base_url="{creds_url}/openai/v1"` で渡す |
| 送る先 | `POST /v1/responses`(Responses API だけ) |
| 鍵 | `OPENAI_API_KEY` を `Authorization: Bearer` で送る。経路は `header: Authorization`、`format: "Bearer {value}"` |
| MCP | `-c mcp_servers.<name>.url="{mcp_url}/<name>"`(Streamable HTTP) |
| 止めておく通信 | `-c check_for_update_on_startup=false`、`-c analytics.enabled=false` |

- **ChatGPT のプランでのログイン**: proxy に持たせる方法は無い。
  - トークンは `~/.codex/auth.json` か OS の keyring に入り、`auth.openai.com` で更新される。
  - サンドボックスの中でログインすると、agent がトークンを読める。
  - API の鍵を使うこと。
- **Codex 自身のサンドボックス**: nsctl のサンドボックスの中で入れ子になる。動かないときは `--sandbox danger-full-access` にする(未確認)。外側の nsctl の境界は変わらない。
- 出典: [config-advanced](https://learn.chatgpt.com/docs/config-file/config-advanced)、[auth](https://learn.chatgpt.com/docs/auth)、[non-interactive](https://learn.chatgpt.com/docs/non-interactive-mode)、[MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)、[model-provider-info](https://github.com/openai/codex/blob/main/codex-rs/model-provider-info/src/lib.rs)

### Google Gemini CLI

| | |
|---|---|
| 1 回の実行 / 対話 | `gemini -p "<prompt>"` / `gemini` |
| API の宛先 | `GOOGLE_GEMINI_BASE_URL: "{creds_url}/gemini"` |
| 送る先 | `POST /v1beta/models/<model>:streamGenerateContent?alt=sse` など |
| 鍵 | `GEMINI_API_KEY` を `x-goog-api-key` で送る。経路は `header: x-goog-api-key` |
| MCP | `~/.gemini/settings.json`(agent の `home` の中)に `{"mcpServers": {"<name>": {"httpUrl": "${NSCTL_MCP_URL}/<name>"}}}`。コマンドで渡す方法は無い |
| 止めておく通信 | 同じ `settings.json` に `"privacy": {"usageStatisticsEnabled": false}`、`"general": {"enableAutoUpdate": false, "enableAutoUpdateNotification": false}` |

- 宛先は https か loopback でなければならない。`http://gemini.creds.internal` は Gemini CLI が拒否するので、`{creds_url}` を使う。
- Gemini CLI は `settings.json` の文字列の中の `${VAR}` を環境変数で置き換える(レビューでの指摘による。未確認)。`${NSCTL_MCP_URL}` を使えば、macOS で port が変わっても書き直さずに済む。効かないときは、中で `env | grep NSCTL_MCP_URL` を見て URL を書く。
- **Google アカウントでのログイン**: proxy に持たせる方法は無い。
  - 通信先は Code Assist(`cloudcode-pa.googleapis.com`)で、渡せるトークンは短い時間で切れる。
  - API の鍵を使うこと。
- 出典: [configuration](https://github.com/google-gemini/gemini-cli/blob/main/docs/reference/configuration.md)、[MCP](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md)、[authentication](https://github.com/google-gemini/gemini-cli/blob/main/docs/get-started/authentication.mdx)、[contentGenerator.ts](https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/core/contentGenerator.ts)

### Aider

| | |
|---|---|
| 1 回の実行 / 対話 | `aider --message "<prompt>" --yes-always` / `aider` |
| API の宛先 | `OPENAI_API_BASE: "{creds_url}/openai/v1"`、`--model openai/<モデル>` |
| 送る先 | `POST /v1/chat/completions` |
| 鍵 | `OPENAI_API_KEY` を `Authorization: Bearer` で送る |
| MCP | 無い |
| 止めておく通信 | `--no-check-update`、`--analytics-disable` |

- モデルの情報を `raw.githubusercontent.com` と `openrouter.ai` から取りに行く。`network.allow` に無いので拒否され、監査ログに出る。動作に要るなら、その 2 つを許すかを決める(未確認)。
- 出典: [options](https://github.com/Aider-AI/aider/blob/main/aider/website/docs/config/options.md)、[OpenAI 互換](https://github.com/Aider-AI/aider/blob/main/aider/website/docs/llms/openai-compat.md)、[analytics](https://github.com/Aider-AI/aider/blob/main/aider/website/docs/more/analytics.md)

### OpenCode

例には入れていない。設定の形が V2 で変わったばかりで、確かなことが少ないため。

- 1 回の実行は `opencode run "<prompt>"`、対話は `opencode`。
- `opencode.json` で、provider の `baseURL` を `{creds_url}/openai/v1` に向ける。値はファイルに書き写すので、macOS では port に注意する。
- MCP は `"mcp": {"servers": {"<name>": {"type": "remote", "url": "http://127.0.0.1:3129/mcp/<name>"}}}`。
- `HTTP_PROXY` と `NO_PROXY` に従う。
- 出典: [providers](https://opencode.ai/docs/providers.md)、[MCP](https://opencode.ai/docs/mcp-servers/)、[network](https://opencode.ai/docs/network/)

## サブスクリプション(定額プラン)のまとめ

| agent | 定額プランのまま、proxy に鍵を持たせられるか |
|---|---|
| Claude Code(Max / Pro) | できる。`claude setup-token` の長く使えるトークン([claude-code.md](claude-code.md)) |
| Codex CLI(ChatGPT) | できない。トークンが短く、agent の中で更新される |
| Gemini CLI(Google アカウント) | できない。同上 |
| Aider | 定額プランのログインが無い |
| OpenCode(ChatGPT、Copilot) | 未確認 |

できないものを定額プランで使うには、サンドボックスの中でログインするしかない。その場合、agent が自分のトークンを読める。その namespace では「agent がトークンを持ち出せる」と分かったうえで使うこと。
