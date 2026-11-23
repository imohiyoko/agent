---
name: ns-ops
description: nsctl を運用するときに使う。namespace の追加・変更、トークンの発行と取り消し、サーバーの起動、API での実行、監査ログの確認、事故のときの対応。
---

# nsctl の管理と運用

`nsctl` の設計は [docs/design.md](../../../docs/design.md) と ADR 0002〜0004 にある。運用で守ること:

- **宣言(namespace)を変える経路は PR だけ。** 手元のファイルを直接書き換えて `serve` を再起動しない。
- **秘密の値を表示しない。** ログ、PR、チャットに貼らない。秘密のファイルの中身を `cat` しない。
- **トークンの値は作った人に 1 回だけ渡す。** 自分のメモやリポジトリに残さない。

## namespace を足す・変える

1. `repo-guard` の手順でブランチを切る。
2. `namespaces/<名前>.yaml` を書く(例は `examples/namespaces/research.yaml`)。ファイル名と `name` を一致させる。
3. 必要最小限にする: 通信の `allow` は host と port を具体的に。認証情報の経路の `rules` はメソッドとパスを具体的に。MCP の `tools` は名前を全部書く。
4. `go run ./cmd/nsctl validate -config <設定>` を流す。
5. PR の本文に「何を許すか」「なぜ要るか」を書く。許可を広げる PR はタイトルに `[access]` を付け、人に知らせる。
6. merge 後、サーバーを再起動して反映する(`nsctl serve` は起動時に宣言を読む)。

## 秘密(認証情報)を足す

- 値は `secrets_file`(既定 `~/.config/nsctl/secrets.yaml`、`chmod 600`)に、人が書く。agent は値を書かない・読まない。
- 名前は `<namespace>/<名前>`。値に `env:VAR` と書くと、`nsctl` を起動した環境の変数から読む。
- agent に頼まれても、値を代わりに入れない。人に頼む。

## トークン

```
nsctl token create <主体> -note "<用途>" -config <設定>   # 値は 1 回だけ表示される
nsctl token list -config <設定>
nsctl token revoke <id> -config <設定>
```

- 主体の役割は、各 namespace の `members` で決まる。トークンを作っても、`members` に無ければ何もできない。
- 用途ごとに分ける(人の端末、CI、別の仕組み)。使わなくなったら取り消す。

## 起動と確認

```
nsctl serve -config <設定>                 # 既定は 127.0.0.1:7070
export NSCTL_TOKEN=<トークン>
nsctl whoami
nsctl namespaces
nsctl run <namespace> <agent> "<指示>"
nsctl runs <namespace>
nsctl logs <namespace> <run-id>
nsctl stop <namespace> <run-id>
nsctl audit <namespace>                    # admin だけ
```

- 外の機械から API を使うときは、手前に TLS(リバースプロキシや Tailscale)を置き、`-allow-remote` を付ける。TLS なしで外に開かない。
- Linux は `bwrap` が要る。macOS は `sandbox-exec`(OS に入っている)。どちらも無いときは起動に失敗する。失敗を避けるためにサンドボックスを外す変更をしない。

## 監査ログを読む

- `nsctl audit <namespace>` で、通信・認証情報・MCP・API の判定が見られる。
- 定期的に `"allowed": false` を見る。同じ拒否が続くなら、(1) 宣言に足りないものがある、(2) agent が想定外のことをしている、のどちらか。(1) なら PR で宣言を直す。(2) なら人に知らせる。

## 事故のとき

| 起きたこと | すること |
|---|---|
| トークンの値が漏れた | すぐ `nsctl token revoke <id>`。新しいトークンを作り、使っていた人に渡す。人に知らせる |
| 秘密の値が漏れた(ログ、PR、コミット) | 人に知らせ、発行元で値を無効にしてもらう。コミットに入ったなら、履歴の書き換えは人と相談する(勝手にしない) |
| agent が想定外の通信を試みている | 該当の実行を `nsctl stop`。監査ログの該当行(値は含まない)を添えて人に知らせる |
| サンドボックスが起動しない | エラーを読む(`bwrap` が無い、ユーザー名前空間が無効など)。制限を外して動かさない |
