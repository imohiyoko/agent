# ADR(設計の決定)

設計の決定を 1 件 1 ファイルで残します。ファイル名は `NNNN-<短い説明>.md`。
状態は Proposed(案)→ Accepted(人が決定)→ Superseded(置き換えられた)。Accepted にできるのは人だけです。

| 番号 | 題 | 状態 |
|---|---|---|
| [0001](0001-openshell-and-namespace-layer.md) | 実行の土台に OpenShell を使い、namespace の層だけを自分で作る | Proposed |
| [0002](0002-lightweight-native-sandbox-and-egress-proxy.md) | 既定の実行は OS の軽いサンドボックスと自前の proxy にし、OpenShell は強い分離が要るときの選択肢にする | Proposed |
| [0003](0003-authn-authz-and-external-api.md) | 認証は人と仕組みだけに持たせ、namespace ごとの役割で認可し、外からの指示は API で受ける | Proposed |
| [0004](0004-per-namespace-mcp-gateway.md) | MCP は namespace ごとの gateway を通し、使える server と tool を namespace で切り替える | Proposed |
