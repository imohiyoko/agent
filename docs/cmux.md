# cmux と組み合わせる

[cmux](https://cmux.com) は、AI の coding agent を何個も並べて使うための macOS の端末。nsctl は cmux の代わりではなく、cmux の中で使う。

| | cmux | nsctl |
|---|---|---|
| 得意なこと | ワークスペースとペインの並べ方、agent が終わったときの通知、ブラウザ、CLI と socket での操作 | namespace ごとのサンドボックス、通信の制限、認証情報を agent に渡さないこと、MCP の tool の制限、監査ログ |
| 分離・通信の制限 | 持たない | 持つ |

出典(確認済み、2026-11-23): [cmux の Claude Code の頁](https://cmux.com/agents/claude-code.md)、[claude-teams の記事](https://cmux.com/blog/cmux-claude-teams.md)。cmux のソースは [manaflow-ai/cmux](https://github.com/manaflow-ai/cmux)。

## 使い方

1 つの namespace に 1 つの cmux のワークスペースを割り当て、そのペインで `nsctl shell` を開く。

```sh
# cmux のワークスペース「personal」のペインで
nsctl shell personal
```

- 見た目(並べ方、サイドバー、通知)は cmux、境界(通信、認証情報、ファイル、MCP)は nsctl が受け持つ。
- `nsctl serve` は cmux の外で常駐させる(launchd、`deploy/launchd/`)。cmux を閉じても、ほかの namespace の実行は止まらない。
- ペインを閉じると、その対話の実行は止まる。

## 分かっていること・いないこと

- **確認済み**: `nsctl shell` は呼んだ端末をそのまま agent に渡す(Linux で疑似端末を使って確かめた)。
- **推測**(未確認): 次の 2 つは、cmux の中でもそのまま働くはず。cmux は macOS のアプリで、この環境では動かせないので確かめていない。
  - 端末のベル(BEL)。agent の端末は cmux のペインそのもの。
  - 画面の表示。
- **できない**: サンドボックスの中の agent からは、cmux の socket と `cmux` の CLI は使えない。
  - cmux の socket は、サンドボックスが見せない場所にある。macOS ではネットワークも閉じている。
  - そのため、Claude Code の hook から `cmux notify` を呼ぶ通知は届かない。
  - 通知には、端末のベルを使う(Claude Code の設定 `preferredNotifChannel` を `terminal_bell` にする。未確認)。
- **しない**: cmux の `claude-teams`(teammate を cmux のペインに出す機能)を、namespace の中から使うこと。teammate のペインを作るには cmux の socket が要る。
