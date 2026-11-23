# ADR 0002: 既定の実行は OS の軽いサンドボックスと自前の proxy にし、OpenShell は強い分離が要るときの選択肢にする

- 状態: Proposed
- 日付: 2026-11-23
- 一部を置き換えるもの: ADR 0001 の「箱には OpenShell を使う」(既定ではなく、選べる実行方式の 1 つにする)
- きっかけ(人の依頼、2026-11-23): より軽量にする方法を考えて実装する。動作環境は macOS と Linux。

## 要旨

- **困りごと**: OpenShell は Docker・Podman・VM のどれかを要する。ノート PC で namespace を何個も動かすには重く、起動も遅い。
- **決定**: 既定の実行方式(`native`)は、OS に元からあるサンドボックスで agent のプロセスを包むだけにする。
  - macOS: `sandbox-exec`(Seatbelt)。
  - Linux: `bwrap`(bubblewrap)。ネットワークの namespace を分ける(`--unshare-net`)。
- 外への通信は、namespace ごとの proxy(`nsctl` が自分で持つ)だけを通す。サンドボックスからは、その namespace の proxy にしか繋がらない。
- OpenShell は、Landlock・seccomp・VM まで要るときの実行方式(`openshell`)として後で足す。宣言の書式は共通にする。

## 決定

### 1. namespace の境界は「どの入口に繋がったか」で決める

agent に名乗らせない。サンドボックスが繋がれる入口(proxy と MCP の口)を namespace ごとに分け、**繋がってきた入口で namespace を決める**。

| | Linux | macOS |
|---|---|---|
| 外への通信を止める | `--unshare-net`(サンドボックスの中には loopback しかない) | Seatbelt で `network-outbound` を拒否し、自分の namespace の proxy の port だけ許す |
| proxy への入口 | namespace ごとの unix socket を、そのサンドボックスにだけ bind する。中では `nsctl sandbox-init` が `127.0.0.1:3128` で受けて socket に渡す | namespace ごとに loopback の port を分ける |
| ほかの namespace の入口 | `nsctl` の状態の置き場所を tmpfs で隠すので見えない | Seatbelt が別の port を許さない |
| `nsctl` の API | 届かない(ネットワークが別) | Seatbelt が許さない |
| 認証情報のファイル | 状態の置き場所ごと隠す | `file-read*` を拒否する |

### 2. proxy の動き

- **既定は拒否**。宣言の `network.allow` に当たる宛先(host と port)だけを通す。`deny` は `allow` より強い。
- HTTPS は `CONNECT` で中継するだけで、中身は見ない(TLS を破らない)。したがって HTTPS ではメソッドとパスでは絞れない。
- メソッドとパスで絞りたい通信と、認証情報を付けたい通信は、**認証情報の経路(credential route)** を使う。agent は `http://<名前>.creds.internal/...` に平文で送り、proxy が規則を確かめ、認証情報のヘッダーを付けて、本当の宛先(`https://...`)に TLS で送る。agent は認証情報の値を一度も持たない。
- 判定はすべて namespace ごとの監査ログ(JSON Lines)に残す。
- `nsctl` 自身が直接外に出られない環境では、設定の `upstream_proxy` で上流の proxy を通す。そのときも、loopback などへの接続の拒否は、宛先の名前を自分で引いて確かめる(上流の proxy が引き直す間に答えが変わる場合は防げない)。
- TLS を破る方式(自前の CA を agent に信頼させる)は採らない。CA の管理が重く、agent の中の証明書の扱いが道具ごとに違うため。

### 3. ファイル

- 書き込めるのは、宣言の `filesystem.writable` と一時ディレクトリだけ。
- 読めないもの: 宣言の `filesystem.deny_read` と、`nsctl` の状態の置き場所(トークン、認証情報、ほかの namespace の socket)。後者は宣言で外せない。

### 4. 限界(はっきり書いておく)

- **同じ OS ユーザーで動く**。サンドボックスの外に出られる穴(OS の不具合、許した unix socket)があれば、人と同じ権限になる。
- macOS の `sandbox-exec` は Apple が「非推奨」と表示しているが、macOS 15 でも動き、Claude Code のサンドボックスも同じ仕組みを使っている(**確認済み**: Claude Code docs「Sandboxing」)。将来なくなったときは `openshell` の実行方式に移る。
- Linux の `bwrap` は、ユーザー名前空間が使えない環境(一部のコンテナの中)では動かない。そのときは起動を止める(制限なしで動かさない)。
- HTTPS の中身(パス)は、認証情報の経路を通さない限り絞れない。

## 確認の状態(2026-11-23)

- **確認済み**: Ubuntu 24.04 で `bwrap --unshare-net` の中は外に繋がらず、loopback に bind できる(実行して確かめた)。
- **未確認**: macOS の Seatbelt の規則(`remote ip "localhost:<port>"`)を実機で動かしていない。生成する規則はテストで確かめるが、実機の確認は人に頼む。

## 検討した代替案

| 案 | 利点 | 欠点 |
|---|---|---|
| OpenShell を既定にする(ADR 0001) | Landlock・seccomp・VM で強い | Docker か VM が要り、重い。0.1.x で書式が変わりうる |
| Docker のコンテナで分ける | どの OS でも同じ形 | macOS では VM が要り重い。ファイルの共有が遅い |
| TLS を破る proxy | HTTPS でもパスで絞れ、認証情報を付けられる | CA の管理と、道具ごとの信頼設定が重い |
