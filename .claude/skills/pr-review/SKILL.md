---
name: pr-review
description: このリポジトリの PR(自分のものを含む)をレビューするときに使う。docs/review.md の観点で差分を確かめ、重さ付きの指摘を残す。
---

# PR のレビュー

観点の本体は [docs/review.md](../../../docs/review.md)。ここはその進め方。

## 手順

1. 差分を取る: `gh pr diff <番号>`(自分の変更なら `git diff origin/main...HEAD`)。
2. PR の本文と、関係する ADR(`docs/adr/`)を読む。PR が言っていることと差分が一致しているか確かめる。
3. docs/review.md の 1〜7 を上から順に見る。1(境界を緩めていないか)に一番時間を使う。具体的には:
   - 宣言の検査(`internal/spec/spec.go` の `Validate`)が緩んでいないか。
   - proxy(`internal/egress`)が、拒否の前に通信してしまう順序になっていないか。loopback などへの接続の拒否が残っているか。
   - サンドボックス(`internal/sandbox`)の引数・規則から、拒否や隠す設定が消えていないか。失敗したときに制限なしで動く分岐が増えていないか。
   - MCP gateway(`internal/mcpgw`)で、許していない tool やメソッドが server に届く経路ができていないか。MCP server が使う認証情報の経路を、agent が直接呼べるようになっていないか(gateway-only)。
   - API(`internal/server/api.go`)の新しい入口が、`s.ns(<操作>, ...)` を通っているか。
4. 疑わしい点は、実際に確かめる。テストを書いて流すか、`bash scripts/check.sh` を流す。確かめられなければ「未確認」と書く。
5. 指摘を残す。

## 指摘の書き方

- 重さ: **must**(merge の前に直す)/ **should**(直した方がよい)/ **nit**(好み)。
- 「どんな入力・状態で、何が間違うか」を書く。「危ないかも」で終わらせない。
- GitHub に書くときは `gh pr review <番号> --comment --body ...` か、行への comment にする。approve はしない(`--approve` を使わない)。
- 指摘が無ければ、「1〜7 を見て指摘なし。確かめたこと: ...」と書く。

## must にするもの

- 既定の拒否が崩れる、namespace をまたげる、秘密が出る、サンドボックスなしで動く経路ができる。
- テストを消す・スキップする。
- 宣言を変える経路が PR 以外に増える(API で宣言を変える、など)。
