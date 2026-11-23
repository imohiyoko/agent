# ADR 0001: 実行の土台に OpenShell を使い、namespace の層だけを自分で作る

- 状態: Accepted(2026-11-23、imohiyoko の委任により agent が決定)。「既定の箱は OpenShell」の部分は [ADR 0002](0002-lightweight-native-sandbox-and-egress-proxy.md) で置き換えた(OpenShell は強い分離が要るときの実行方式として残す。未実装)
- 日付: 2026-11-23
- 決めた人: imohiyoko(土台は OpenShell。2026-11-23 に選択)。この ADR の中身(namespace の定義など)はまだ案

## 要旨

- **困りごと**: herdrOrchestrator では「AI(agent)が何をしてよいか」をアプリの中で判定していた(herdrOrchestrator の ADR 0007)。これは agent と同じ OS ユーザー・同じネットワークの上での判定なので、agent が判定を通らない経路(別のコマンド、別の言語、直接の通信)を使うと外れる。herdr にも pane を分離する機能が無い(同 ADR 0001)。
- **決定**: agent を **namespace** という箱に入れ、認証とネットワークの制限を **箱の外側で** 強制する。箱には NVIDIA OpenShell のサンドボックスを使う。自分で作るのは「namespace の定義を OpenShell の設定(ポリシー・provider・サンドボックス)に変える層」だけにする。
- **作らないもの**: サンドボックスの分離、通信の検査、認証情報の注入。これらは OpenShell に任せる。

## 状況(Context)

やりたいこと(人の言葉、2026-11-23): agent を namespace で分けることで AI に認証認可を付けるのではなく、namespace ごとに認証したり、ネットワークごとに制限したりする仕組みを作りたい。

言い換えると:

- 認可の単位は agent 1 体ではなく namespace。同じ namespace の agent は同じ境界を持つ。
- 「誰がその namespace を操作できるか」(人・仕組みの認証)と、「その namespace から何に通信できるか・どの認証情報が使えるか」を、namespace ごとに決める。
- 強制は agent の協力に頼らない(agent が持っていない能力は使えない)。

## OpenShell について分かっていること

確認日 2026-11-23、OpenShell v0.1.9(ドキュメントの表示)。出典は [README](https://github.com/NVIDIA/OpenShell) と docs.nvidia.com/openshell/latest の各ページ。

### 確認済み

- ポリシーは YAML(`version: 1`)。項目は `filesystem_policy`、`landlock`、`process`、`network_policies`、`network_middlewares`。ファイルと process の規則は作成時に決まり、ネットワークの規則は動いている間に変えられる。(出典: policies/overview)
- 通信は既定で拒否。`network_policies` の規則が許したものだけ外に出られる。規則は「宛先(host・port)」と「通信してよいプログラム(binary のパス)」の組。`protocol`(rest、websocket、graphql、mcp など)を書くと、HTTP のメソッドとパスでも絞れる。`deny_rules` は許可より強い。(出典: policies/network-rules)
- **注意**: 規則の `enforcement` の既定は `audit`(記録するだけで止めない)。止めるには `enforce` を明示する必要がある。(出典: 同)
- 認証情報は「provider」として gateway に登録する。サンドボックスには本物の値ではなく置き換え用の値が渡り、proxy が「ポリシーが許した宛先」かつ「provider に結び付いた宛先」への通信にだけ本物の値を入れる。合わないと HTTP 403(`credential_endpoint_mismatch`)。(出典: providers/overview)
- gateway が制御の中心で、サンドボックスの作成、provider の保管、ポリシーの配布を行う。実行環境は Docker、Podman、Kubernetes、MicroVM。(出典: gateways/overview、about/architecture)
- gateway 自体に認証がある。mTLS、OIDC(役割 `openshell-admin` / `openshell-user`)、手前の認証(Cloudflare Access など)、ローカル開発向けの認証なし。user の役割の操作は「workspace の member であること」が条件。(出典: gateways/authentication)
- gateway の管理者は、全サンドボックスに効く global policy を掛けられる。各サンドボックスのポリシーより優先される。(出典: gateways の関連ページ)
- 危ないポリシーの変更は、人の確認に回る仕組みがある。(出典: policies の関連ページ)
- Linux では Landlock(ファイル)、seccomp(ネットワーク)、root でない権限なしのユーザーで分離する。(出典: about/architecture)

### 未確認・推測

- **未確認**: OpenShell の「workspace」が、こちらの namespace にそのまま使えるか(複数のサンドボックスと provider をまとめ、member を持てるか)。workspace の説明のページはまだ読んでいない。**これが namespace の層の作り方を一番大きく左右する。**
- **未確認**: 1 つのポリシーファイルを、複数のサンドボックスで共有する正式な方法(global policy 以外)。
- **未確認**: macOS(Apple Silicon)の上での分離の強さ。サンドボックスの中は Linux(Docker か VM)になるはず(推測)。
- **未確認**: Claude Code を中で動かしたときの使い勝手(git の SSH、`gh`、MCP サーバー)。
- **推測**: 0.1.x なので、設定の書式が変わる可能性がある。層を薄くし、OpenShell の書式を直接書く箇所を 1 箇所に集める必要がある。

## 決定(案)

### 1. namespace の定義

namespace は次の 5 つの組。リポジトリの中に宣言として置き、人が PR で変える。

| 項目 | 中身 | OpenShell の何になるか |
|---|---|---|
| members | その namespace を操作できる人・仕組み(サンドボックスの作成、停止、ポリシーの変更) | gateway の認証(OIDC / mTLS)と workspace の member(未確認。だめなら層が判定する) |
| network | 通信してよい宛先とプログラム。メソッド・パスまで絞る | その namespace の全サンドボックスの `network_policies`。`enforcement: enforce` を層が必ず付ける |
| credentials | その namespace で使える認証情報の名前(値はリポジトリに置かない) | provider。名前は `<namespace>--<名前>` |
| filesystem / process | 読み書きしてよい場所、動かすユーザー | `filesystem_policy`、`landlock`、`process` |
| agents | その namespace で動かす agent の一覧(イメージ、起動コマンド) | サンドボックス。名前は `<namespace>--<agent>` |

agent 1 体ごとの権限は作らない。違う権限が要る agent は、別の namespace に入れる。

### 2. 層がすること

- `namespace` の宣言を読み、OpenShell の設定に変えて適用する(`apply`)。適用の前に差分を見せる(`plan`)。
- 宣言と、gateway の実際の状態のずれを見つける(`drift`)。ずれは直さずに報告する。
- 宣言の検査: `enforcement` が `audit` の規則、宛先が `*` の規則、namespace をまたぐ provider の参照を拒否する。
- 組織全体で禁止するもの(例: 決まった宛先への通信)は global policy にまとめる。

### 3. 層がしないこと

- サンドボックスの中の検査、通信の proxy、認証情報の保管。OpenShell の仕事。
- agent の仕事の管理(タスク、報告、承認)。herdrOrchestrator の taskboard を使い続ける(下の「herdrOrchestrator との関係」)。

### 4. 実装の言語と形

- 1 つのバイナリの CLI にする(仮称 `nsctl`)。Go を第一候補にする(herdrOrchestrator の `herdrctl` と同じ配り方ができる)。**人が決めること 2**。
- OpenShell との境目は、`openshell` CLI を呼ぶか、gateway の API を呼ぶかのどちらか 1 つの部品に閉じ込める。

### 5. herdrOrchestrator との関係

- 第 1 段では関係を持たない。単独で「namespace を作り、その中で agent を 1 体動かす」ところまで作る。
- 第 2 段で、herdrOrchestrator の agent の起動先を、herdr の pane から namespace のサンドボックスに替えられるようにする。

## 段階

1. 調査: workspace の仕組みを読み、ローカルの macOS で gateway とサンドボックスを 1 つ動かす。結果をこの ADR の「未確認」に書き戻す。
2. 宣言の書式と検査(`nsctl validate`)。OpenShell に触らない。
3. `plan` / `apply` / `drift`。namespace 1 つ、agent 1 体。
4. 複数の namespace。namespace をまたいで通信・認証情報が使えないことをテストで示す。
5. herdrOrchestrator からの起動。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| Kubernetes + kubernetes-sigs/agent-sandbox + NetworkPolicy | 本物の namespace と RBAC。分離が強い(gVisor / Kata) | ノート PC 1 台では重い。認証情報の注入は別に作る |
| Docker + egress proxy を自作(mattolson/agent-sandbox のような形) | 自由に作れる。namespace を最初から入れられる | 分離・proxy・注入を全部保守することになる |
| herdrOrchestrator の policy(アプリの中の判定)を続ける | 追加のコストが無い | agent と同じ境界の中の判定なので、迂回できる |

## 人が決めること

1. この ADR の namespace の定義(5 つの組)でよいか。
2. 実装の言語(Go でよいか)。
3. 第 1 段の調査を先にやるか(推奨。workspace の結果で層の形が変わるため)。

## 関連

- herdrOrchestrator ADR 0001(制限は環境で強制する。L3・L4 の具体化がこの ADR)
- herdrOrchestrator ADR 0007(主体と policy。アプリの中の認可)
