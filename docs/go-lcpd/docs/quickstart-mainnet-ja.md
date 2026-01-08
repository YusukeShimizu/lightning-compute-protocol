# クイックスタート（mainnet）

`openai-serve`（OpenAI互換HTTP）まで含めて “ガイド付きで一式起動” したい場合は、
[lcp-quickstart](/lcp-quickstart/overview-ja) を参照してください。このページは手動の `go-lcpd` 手順です。

## 安全性と運用上の注意（mainnet）

- このプロジェクトは未監査です。mainnet で送金 / チャネル開設 / 請求書支払いを行うと、資金を失う可能性があります。
- まず `regtest-ja.md`（無料 / より安全）から始め、mainnet は少額から試してください。
- オンチェーン手数料・Lightning 手数料、ルーティング失敗、流動性管理はすべて利用者の責任です。

## ゴール

自分の `lnd` を動かし、下記 Provider に接続して Quote → Pay → Result を完了します:

- Provider node: `03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309`

## 前提

- Linux または macOS
- mainnet `lnd`（このリポジトリのスコープ外）が動作しており `lncli` が使えること
- Go 1.24.4+
- `jq`（任意。いくつかの手順で JSON の整形表示にのみ使用）

注: このリポジトリには `lnd/` フォルダやバイナリは含まれません。`lnd` は各自で用意してください。

## 0) `go-lcpd` の CLI ツールをビルド（初回のみ）

このクイックスタートは Nix を使わず、バイナリを `go-lcpd/bin/` に配置します:

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

以降のコマンドは `./bin/lcpd-grpcd` / `./bin/lcpdctl` / `./bin/lcpd-oneshot` を前提とします。

## 1) lnd を起動（mainnet）

お好みの手順で `lnd` を mainnet モードで起動してください。以下が必要になります:

- lnd gRPC アドレス（例: `localhost:10009`）
- TLS 証明書パス（例: `~/.lnd/tls.cert`）
- admin macaroon パス（例: `~/.lnd/data/chain/bitcoin/mainnet/admin.macaroon`）

## 2) ウォレット作成/アンロック（初回）

初回（対話的）:

```sh
lncli create
```

再起動後にアンロックが必要な場合（対話的）:

```sh
lncli unlock
```

## 3) Provider に接続（Lightning peer connect）

```sh
PROVIDER_NODE="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309"
lncli connect "$PROVIDER_NODE"
lncli listpeers
```

## 4) 支払いの準備（資金 + チャネル）

`lcpd-oneshot -pay-invoice` を使うには、ノードが支払える状態である必要があります:

- オンチェーン資金があること
- 少なくとも 1 本の outbound 流動性のあるチャネルがあること（最短は Provider への直結チャネル）

例（説明用。金額/承認回数などは各自で判断してください）:

```sh
PROVIDER_PUBKEY="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983"

# create a deposit address, fund it, and wait for confirmations
lncli newaddress p2wkh
lncli walletbalance

# open a channel to the Provider (may fail depending on Provider policy)
lncli openchannel --node_key "$PROVIDER_PUBKEY" --local_amt 20000
lncli listchannels
```

## 5) go-lcpd を起動（Requester）

```sh
cd go-lcpd

export LCPD_BACKEND=disabled
export LCPD_LOG_LEVEL=debug

export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

## 6) Provider が対応する model を確認（任意）

別ターミナルで:

```sh
cd go-lcpd
./bin/lcpdctl lcpd list-lcp-peers -s 127.0.0.1:50051 -o prettyjson
```

`peers[].remoteManifest.supportedTasks[].openaiChatCompletionsV1.model` に Provider の model（広告されている場合）が入っています。

注意:

- `supportedTasks` は `lcp_manifest` の任意フィールドです。Provider が `llm.models` を設定していない（または Provider モードが disabled）場合、`supported_tasks` を広告しません。
- `-o prettyjson` は空フィールドを省略するため、仕様上フィールドが absent であっても `supportedTasks` が見えない場合があります。

## 7) ジョブを 1 回実行（Quote → Pay → Result）

別ターミナルで:

```sh
cd go-lcpd

PROVIDER_PUBKEY="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983"

./bin/lcpd-oneshot \
  -server-addr 127.0.0.1:50051 \
  -peer-id "$PROVIDER_PUBKEY" \
  -pay-invoice \
  -model gpt-5.2 \
  -prompt "Say hello in one word." \
  -timeout 60s
```

## 8) （任意）対話チャットを開始

ローカルに会話ログを保持し、各ターンのプロンプトに含めます。各ターンで請求額と累計が表示されます:

```sh
cd go-lcpd

PROVIDER_PUBKEY="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983"

./bin/lcpd-oneshot \
  -server-addr 127.0.0.1:50051 \
  -peer-id "$PROVIDER_PUBKEY" \
  -pay-invoice \
  -model gpt-5.2 \
  -chat
```

## トラブルシューティング

- `peer is not ready for lcp`: `lnd` が Provider に接続できているか（`lncli listpeers`）確認し、`lcpdctl list-lcp-peers` でも見えることを確認してください。
- `lcp_error code=2: unsupported model ...`: `-model` を Provider がサポートする値にしてください（手順 6 を参照）。
- `payment failed`: チャネル / ルート / 流動性が不足している可能性があります。`lncli walletbalance` と `lncli listchannels` を確認してください。
