# リポジトリの設計

この文書は、このリポジトリが何を作り、どこに何を置くかを書きます。決定の理由は `docs/adr/` にあります。

## 作るもの

agent を **namespace** に分け、namespace ごとに次の 2 つを箱の外側で強制する仕組み。

- **認証**: その namespace を操作できる人・仕組みを決める。agent 自身には認証を持たせない。
- **ネットワークと認証情報**: その namespace から通信できる宛先と、使える認証情報を決める。

既定の箱は OS の軽いサンドボックス(macOS は `sandbox-exec`、Linux は `bwrap`)と、`nsctl` 自身の proxy・MCP gateway([ADR 0002](adr/0002-lightweight-native-sandbox-and-egress-proxy.md)〜[0004](adr/0004-per-namespace-mcp-gateway.md))。より強い分離が要るときの実行方式として OpenShell を後で足す([ADR 0001](adr/0001-openshell-and-namespace-layer.md))。

```
人(PR で宣言を変える)
   │
   ▼
namespaces/<名前>.yaml ── nsctl validate
   │
   ▼
nsctl serve ── API(トークンで認証、namespace ごとの役割で認可) ◀── 人・CI・ほかの仕組み
   │
   ├── namespace A: proxy(A の通信規則・A の認証情報)、MCP gateway(A の tool)
   │        ▲ A のサンドボックスからはここにだけ繋がる
   │        └── agent の実行(sandbox-exec / bwrap)
   │
   └── namespace B: proxy、MCP gateway ── B のサンドボックス
```

## 置き場所

| パス | 中身 |
|---|---|
| `AGENTS.md` / `CLAUDE.md` | ここで作業する agent の決まり |
| `.claude/skills/` | agent 向けの手順(治安維持 `repo-guard`、レビュー `pr-review`、ADR `adr-write`、運用 `ns-ops`) |
| `scripts/check.sh` | CI と同じ検査。PR の前に手元で流す |
| `docs/` | 設計(この文書)、レビューの観点、ADR |
| `examples/` | 設定と namespace の宣言の例 |
| `cmd/nsctl/` | CLI の入口(サーバー、トークン、API の client、`sandbox-init`) |
| `internal/spec/` | 宣言の読み込みと検査。ほかの部品を知らない |
| `internal/auth/` | API のトークン(認証)と、役割による認可(ADR 0003) |
| `internal/egress/` | namespace ごとの proxy と認証情報の経路(ADR 0002) |
| `internal/sandbox/` | macOS(`sandbox-exec`)と Linux(`bwrap`)のサンドボックス(ADR 0002) |
| `internal/mcpgw/` | namespace ごとの MCP gateway(ADR 0004) |
| `internal/server/` | 上を組み合わせる。namespace ごとの入口、実行の管理、API |
| `internal/secrets/`、`internal/audit/` | 秘密の値の取り出しと、監査ログ |
| `internal/openshell/` | (未実装)強い分離が要るときの実行方式。OpenShell の書式はここにだけ書く |

言語は Go(1 つのバイナリで macOS と Linux に配れるため)。

## 守る性質

設計を変えても、次の性質は崩さない。崩す変更は ADR が要る。

1. **既定は拒否。** 宣言に書いていない通信・認証情報・書き込みは許さない。
2. **止める規則にする。** 記録するだけの方式で適用しない。拒否したら通信を止める(OpenShell を使うときも `enforce` にする)。
3. **namespace をまたがない。** ある namespace の宣言から、別の namespace の provider やサンドボックスを参照できない。
4. **値を持たない。** リポジトリにも `nsctl` の出力にも、認証情報の値を出さない。名前だけを扱う。
5. **境目は 1 つ。** OpenShell の書式とコマンドは `internal/openshell/` の外に書かない。OpenShell が 0.1.x で変わりうるため。
6. **宣言を変える経路は PR だけ。** API や実行中の agent から宣言を変える経路を作らない。
7. **サンドボックスなしで動かさない。** サンドボックスが使えないときは止まる。

## テストの方針

- 宣言の検査(`internal/spec/`)は単体テストで、性質 1〜4 をそれぞれ破る宣言を拒否することを示す。
- proxy、MCP gateway、認可はそれぞれ単体テストを持つ。
- `internal/server/` の結合テストは、Linux では本物の `bwrap` の中で agent を動かし、認証情報の経路・通信の拒否・隠したファイル・MCP を確かめる。macOS の CI では `sandbox-exec` の中で、書き込みと通信の拒否を確かめる。
- テストは `bash scripts/check.sh` でまとめて流す。CI も同じものを流す。
