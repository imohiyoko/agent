# リポジトリの設計

この文書は、このリポジトリが何を作り、どこに何を置くかを書きます。決定の理由は `docs/adr/` にあります。

## 作るもの

agent を **namespace** に分け、namespace ごとに次の 2 つを箱の外側で強制する仕組み。

- **認証**: その namespace を操作できる人・仕組みを決める。agent 自身には認証を持たせない。
- **ネットワークと認証情報**: その namespace から通信できる宛先と、使える認証情報を決める。

箱(サンドボックス)と通信の検査は NVIDIA OpenShell を使い、このリポジトリは「namespace の宣言を OpenShell の設定に変える層」を作る([ADR 0001](adr/0001-openshell-and-namespace-layer.md))。

```
人(PR で宣言を変える)
   │
   ▼
namespaces/<名前>/   ──  nsctl validate / plan / apply / drift
   │                         │
   │                         ▼
   │                OpenShell gateway(認証・provider・ポリシー)
   │                         │
   ▼                         ▼
                 namespace A のサンドボックス群   namespace B のサンドボックス群
                 (A の通信規則・A の認証情報)     (B の通信規則・B の認証情報)
```

## 置き場所(予定)

| パス | 中身 | いつ作るか |
|---|---|---|
| `AGENTS.md` / `CLAUDE.md` | ここで作業する agent の決まり | 最初 |
| `docs/design.md` | この文書 | 最初 |
| `docs/review.md` | レビューの観点 | 最初 |
| `docs/adr/` | 設計の決定 | 最初 |
| `namespaces/` | namespace の宣言(例と、実際に使うもの) | 段 2 |
| `schema/` | 宣言の書式(JSON Schema) | 段 2 |
| `cmd/nsctl/` | CLI の入口 | 段 2 |
| `internal/spec/` | 宣言の読み込みと検査。OpenShell を知らない | 段 2 |
| `internal/openshell/` | OpenShell との境目。OpenShell の書式とコマンドはここにだけ書く | 段 3 |
| `internal/plan/` | 宣言と実際の状態の差分 | 段 3 |
| `tests/` | 結合テスト(本物の gateway を使うもの)。普段のテストは偽物を使う | 段 3 |

言語は Go を第一候補にしている(ADR 0001 の「人が決めること」2)。

## 守る性質

設計を変えても、次の性質は崩さない。崩す変更は ADR が要る。

1. **既定は拒否。** 宣言に書いていない通信・認証情報・書き込みは許さない。
2. **止める規則にする。** OpenShell の規則を `audit`(記録だけ)で適用しない。`nsctl` は必ず `enforce` にする。
3. **namespace をまたがない。** ある namespace の宣言から、別の namespace の provider やサンドボックスを参照できない。
4. **値を持たない。** リポジトリにも `nsctl` の出力にも、認証情報の値を出さない。名前だけを扱う。
5. **境目は 1 つ。** OpenShell の書式とコマンドは `internal/openshell/` の外に書かない。OpenShell が 0.1.x で変わりうるため。
6. **人が変える。** 宣言を変える経路は PR だけ。`apply` を agent が自分の判断で走らせる経路を作らない。
7. **ずれは報告する。** `drift` は差分を見せるだけで、勝手に直さない。

## テストの方針

- 宣言の検査(`internal/spec/`)は OpenShell なしで単体テストする。上の性質 1〜4 を、それぞれ破る宣言を拒否するテストで示す。
- `internal/openshell/` は偽の `openshell` コマンドでテストする。本物の gateway を使うテストは別に分け、手元で人が流す。
- 段 4 では「namespace A のサンドボックスから B の宛先・B の認証情報が使えない」ことを、本物の gateway で確かめるテストを置く。
