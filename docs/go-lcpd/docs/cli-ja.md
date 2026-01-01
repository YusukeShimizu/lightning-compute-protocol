# CLI（lcpdctl）— go-lcpd 用 protoc-gen-cobra クライアント

このドキュメントでは以下の使い方を説明します:

- `lcpdctl`: `go-lcpd` の gRPC API（`lcpd.v1.LCPDService`）向けに生成された CLI クライアント
- `lcpd-oneshot`: `RequestQuote` と（任意で）`AcceptAndExecute` を実行する小さなヘルパー

## 前提

- Go 1.24.4+
- `jq`（任意。いくつかの例で JSON の整形表示にのみ使用）

## CLI ツールをインストール（推奨）

バイナリを `go-lcpd/bin/` にインストールします（Nix 不要）:

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

以降の例はすべて:
- `./bin/lcpd-grpcd`
- `./bin/lcpdctl`
- `./bin/lcpd-oneshot`

を使用します。

## 安全性と運用上の注意

- `lcpd-grpcd` は plaintext gRPC（TLS なし / 認証なし）を提供します。`127.0.0.1` にバインドするか、遠隔アクセスが必要なら SSH/VPN/リバースプロキシ等で保護してください。
- `AcceptAndExecute` は `lnd` を使って BOLT11 invoice を支払います。mainnet では実際に資金を消費します。regtest から始め、少額で試してください。
- LCP メッセージングには Provider への Lightning ピア接続が必要であり、Provider 側が LCP（manifest 観測）をサポートしている必要があります。
- サポートされるタスク種別は `llm.chat` のみです。
- Quote の実行は `terms.quoteExpiry` によって時間制限されます。expiry 後の呼び出しは失敗するのが想定挙動です。
- LCP ピアメッセージングには payload 制限（manifest の `max_payload_bytes`）があります。`go-lcpd` のデフォルトは `16384` バイトです。
- `job_id` の CLI フラグは base64（proto の `bytes`）で、protojson のエンコーディングに合わせています。
- `lcpdctl --timeout` は dial timeout（接続確立までの上限）であり RPC の deadline ではありません。長い RPC は Ctrl-C でキャンセルするか、全体の deadline が必要なら `lcpd-oneshot -timeout ...` を使ってください。

古いドキュメントについて:
- 以前のドキュメントでは `CreateQuote → Execute → VerifyReceipt` のような手順名が登場していました。
  現在の `go-lcpd` の公開 gRPC フローは `RequestQuote → AcceptAndExecute` で、invoice binding の検証は `AcceptAndExecute` 内部で行われます。

## 起動（gRPC daemon）

別ターミナルで gRPC daemon を起動します。

### Requester モード（推奨デフォルト）

Requester モードは、ピアメッセージングと invoice 支払いのために `lnd` 接続が必要です:

```sh
cd go-lcpd

export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

### Provider モード（任意）

Provider モードには以下が必要です:
- `LCPD_PROVIDER_CONFIG_PATH`（YAML）
- `LCPD_BACKEND`（例: `openai`）
- `LCPD_LND_*`（Provider もピアメッセージングを使うため）

詳細: `configuration-ja.md`。

### lnd 無効（gRPC のみ）

```sh
cd go-lcpd
./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

daemon 自体は起動しますが、LCP ピア操作（`ListLCPPeers` / `RequestQuote` / `AcceptAndExecute`）には `lnd` が必要です。

## One-shot CLI（`lcpd-oneshot`）

`lcpd-oneshot` は gRPC API の薄いラッパーです:
- 常に `RequestQuote` を実行
- `-pay-invoice=true` のときのみ `AcceptAndExecute` を実行

デフォルト:
- `server-addr=127.0.0.1:50051`
- `profile=gpt-5.2`
- `timeout=30s`

制約:
- `lcpd-oneshot` は insecure gRPC（TLS なし）です。localhost にバインドした `lcpd-grpcd` と合わせて使ってください。
- `peer-id` が必要です（66 hex の compressed pubkey）。

プロンプトの選択優先順位: `--prompt` → 位置引数（スペース結合）→ stdin。

例（text 出力）:

```sh
cd go-lcpd
./bin/lcpd-oneshot \
  -peer-id "<provider_pubkey_hex>" \
  -profile gpt-5.2 \
  -prompt "Say hello in one word."
```

JSON 出力が欲しい場合は `--json` を使ってください。

価格表示について:
- このツールは `price_msat` に加えて、利便のため `price_sat`（10 進文字列、milli-sat 精度）も表示します。

### 対話チャットモード（`-chat`）

`-chat` は簡易 REPL を実行し、会話コンテキストを維持するために「全 transcript」を各ターンのプロンプトに埋め込みます。
各ターンで請求額と累計が表示されます:

- `paid=<sat> sat total=<sat> sat`

使い方:

```sh
cd go-lcpd
./bin/lcpd-oneshot \
  -peer-id "<provider_pubkey_hex>" \
  -profile gpt-5.2 \
  -pay-invoice \
  -chat
```

注意:
- `-chat` には `-pay-invoice=true` が必要です（そうでないとモデル応答を得られません）。
- 終了は `/exit`（または Ctrl-D）です。
- stdin/stdout が TTY のとき、可読性のために `lcpd-oneshot` は Bubble Tea の小さな TUI を使います。それ以外はプレーンな REPL にフォールバックします。
- チャット履歴は `-max-prompt-bytes`（デフォルト `12000`）に収まるよう切り詰められる場合があります。`-max-prompt-bytes=0` で無効化できます（payload 制限に注意）。
- 任意: `-system-prompt "..."` で System 指示を会話の先頭に追加できます。

## lnd 連携（カスタムメッセージ / `lcp_manifest` 交換）

設定されている場合、`go-lcpd` は gRPC で `lnd` に接続し、BOLT #1 のカスタムメッセージで `lcp_manifest` を交換します。
これにより、既存の Lightning ピア接続の上で LCP 対応ピアを自動 discovery できます。
`ListLCPPeers` で一覧できます。
