# agent

agent を **namespace** に分け、namespace ごとに認証・通信・認証情報・MCP の tool を、agent の外側で強制する仕組み。
agent 自身には認証を持たせない。agent ができることは、入れられた namespace の宣言で決まる。

- 設計: [docs/design.md](docs/design.md)、決定: [docs/adr/](docs/adr/)
- ここで作業する agent の決まり: [AGENTS.md](AGENTS.md)
- 動く環境: macOS(`sandbox-exec`)と Linux(`bwrap` が要る)

## 使い方

```
go build -o nsctl ./cmd/nsctl
cp examples/nsctl.yaml . && cp -r examples/namespaces .   # 例から始める
./nsctl validate                                        # 宣言の検査
./nsctl token create <自分の名前>                         # API のトークン(1 回だけ表示)
./nsctl serve                                           # 127.0.0.1:7070 で API を受ける

export NSCTL_TOKEN=<トークン>
./nsctl run research claude "README を要約して"           # namespace の中で agent を動かす
./nsctl runs research
./nsctl logs research <run-id>
```

認証情報の値は `~/.config/nsctl/secrets.yaml`(`chmod 600`)に書く。リポジトリには入れない。

```yaml
research/anthropic: env:ANTHROPIC_API_KEY
research/github: env:GITHUB_TOKEN
```

## 検査

```
bash scripts/check.sh
```
