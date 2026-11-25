# ADR(設計の決定)

設計の決定を 1 件 1 ファイルで残します。ファイル名は `NNNN-<短い説明>.md`。
状態は Proposed(案)→ Accepted(決定)→ Superseded(置き換えられた)。
2026-11-23 に imohiyoko が「ADR は何も聞かなくていい、自由に実装して。その代わりドキュメントはちゃんと残すこと」と、決定を agent に任せたので、agent が Accepted にしてよい([AGENTS.md](../../AGENTS.md) の 1)。

| 番号 | 題 | 状態 |
|---|---|---|
| [0001](0001-openshell-and-namespace-layer.md) | 実行の土台に OpenShell を使い、namespace の層だけを自分で作る | Accepted(既定の箱は 0002 で置き換え) |
| [0002](0002-lightweight-native-sandbox-and-egress-proxy.md) | 既定の実行は OS の軽いサンドボックスと自前の proxy にし、OpenShell は強い分離が要るときの選択肢にする | Accepted |
| [0003](0003-authn-authz-and-external-api.md) | 認証は人と仕組みだけに持たせ、namespace ごとの役割で認可し、外からの指示は API で受ける | Accepted |
| [0004](0004-per-namespace-mcp-gateway.md) | MCP は namespace ごとの gateway を通し、使える server と tool を namespace で切り替える | Accepted |
| [0005](0005-interactive-sessions-and-terminal-multiplexers.md) | 人の端末で agent を対話で動かす(nsctl shell)。端末の道具(cmux など)とは組み合わせて使う | Accepted |
| [0006](0006-agent-neutral-endpoints.md) | どの coding agent でも同じ境界で動かすため、経路と gateway の URL を置き換えと環境変数で渡す | Accepted |
| [0007](0007-split-boundary-and-orchestrator-roles.md) | 境界(認証・通信・認証情報・起動)は nsctl、仕事の割り振りは orchestrator、画面は herdr に分ける | Accepted(決定 3 は 0008 で置き換え) |
| [0008](0008-orchestrator-as-nsctl-api-client-and-run-events.md) | orchestrator は nsctl の API の客にし、実行の途中の報告は nsctl が実行ごとに溜める | Accepted |
