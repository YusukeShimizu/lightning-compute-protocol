# regtest devnet（bitcoind + 2x lnd）手動 runbook

このドキュメントは、ローカルで regtest devnet（Bitcoin Core + 2 つの `lnd` ノード）を動かすための手順です。
`go-lcpd` の Lightning 連携（カスタムメッセージ、invoice binding、支払い）を試すことができます。

この devnet は `./scripts/devnet` で管理されます。
状態/ログは `./.data/devnet/` 配下に保存されます（gitignored）。

手順が失敗したら、`./.data/devnet/` 配下のログ/状態を確認してください。
必要に応じて `LCPD_LOG_LEVEL=debug` で再実行してください。

このドキュメントでは、2 つの役割で最小の Quote → Pay → Result を実行します:

- Alice: Provider。Provider モードは Alice のみ有効化します。外部 API を避けるため `LCPD_BACKEND=deterministic` を使います。
- Bob: Requester。

## 前提

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` が `PATH` にあること
- `lnd` / `lncli` が `PATH` にあること
- `jq` が `PATH` にあること（この手順で使用します）

## go-lcpd の CLI ツールをビルド（初回のみ）

バイナリを `go-lcpd/bin/` にインストールします:

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

## 起動 / 停止

起動:

```sh
cd go-lcpd
./scripts/devnet up
```

ステータス:

```sh
./scripts/devnet status
./scripts/devnet info
```

停止:

```sh
./scripts/devnet down
```

## ウォレット初期化 / アンロック（初回）

`lnd` はウォレット未初期化でも起動できますが、RPC（支払い/チャネル/invoice）を使う前にウォレット作成が必要です。

初回（対話的）:

```sh
./scripts/devnet lncli alice create
./scripts/devnet lncli bob create
```

再起動後にアンロックが必要な場合（対話的）:

```sh
./scripts/devnet lncli alice unlock
./scripts/devnet lncli bob unlock
```

スモークチェック:

```sh
./scripts/devnet lncli alice getinfo
./scripts/devnet lncli bob getinfo
```

## オンチェーン資金投入（Alice を miner にする）

1) Alice の受け取りアドレスを作成:

```sh
ADDR="$(./scripts/devnet lncli alice newaddress p2wkh | jq -r .address)"
echo "$ADDR"
```

2) regtest でブロックを掘って Alice に資金を付与（coinbase maturity のため 101 ブロック掘る）:

```sh
./scripts/devnet bitcoin-cli generatetoaddress 101 "$ADDR"
./scripts/devnet lncli alice walletbalance
```

## ノード接続 / チャネル開設 / 支払い（Bob が Alice に支払える状態にする）

### 1) Bob に接続

```sh
BOB_PUBKEY="$(./scripts/devnet lncli bob getinfo | jq -r .identity_pubkey)"
BOB_P2P_ADDR="$(./scripts/devnet paths bob | awk -F= '/^p2p_addr=/{print $2}')"

./scripts/devnet lncli alice connect "${BOB_PUBKEY}@${BOB_P2P_ADDR}"
./scripts/devnet lncli alice listpeers
```

### 2) Alice → Bob へチャネルを開く（Bob が支払えるように push）

```sh
./scripts/devnet lncli alice openchannel --node_key "$BOB_PUBKEY" --local_amt 200000 --push_amt 10000
./scripts/devnet bitcoin-cli generatetoaddress 3 "$ADDR"
./scripts/devnet lncli alice listchannels
./scripts/devnet lncli bob listchannels

# confirm Bob has enough outbound balance to pay Alice (assumes `push_amt` shows up on Bob)
./scripts/devnet lncli bob channelbalance
```

### 3) Bob が Alice の invoice を支払う（接続性/ルート確認）

```sh
PAY_REQ="$(./scripts/devnet lncli alice addinvoice --amt 1000 | jq -r .payment_request)"
./scripts/devnet lncli bob payinvoice "$PAY_REQ"
```

## go-lcpd を試す（カスタムメッセージ / Quote → Pay → Result）

2 つの `lnd` ノードがピアとして接続できたら、両側で `go-lcpd` を起動します。
これにより BOLT #1 のカスタムメッセージ上で `lcp_manifest` が交換されます。
`ListLCPPeers` で観測できます。

この手順では Alice を Provider として起動し、外部依存なしで `lcp_result` を返します。
`LCPD_BACKEND=deterministic` を使います。

Provider 設定は YAML 優先（`LCPD_PROVIDER_CONFIG_PATH`）です。

### 0) Provider YAML 設定を作成

例: `go-lcpd/provider.devnet.yaml`

```sh
cd go-lcpd
cat > provider.devnet.yaml <<'YAML'
enabled: true
quote_ttl_seconds: 300

llm:
  max_output_tokens: 512
  chat_profiles:
    gpt-5.2:
      price:
        # regtest example pricing (choose any policy you like)
        input_msat_per_mtok: 1
        output_msat_per_mtok: 1
YAML
```

### 1) Alice で go-lcpd を起動（Provider / 別ターミナル）

```sh
cd go-lcpd

export LCPD_BACKEND=deterministic
export LCPD_LOG_LEVEL=debug

export LCPD_PROVIDER_CONFIG_PATH="$PWD/provider.devnet.yaml"

export LCPD_LND_RPC_ADDR="$(./scripts/devnet paths alice | awk -F= '/^rpc_addr=/{print $2}')"
export LCPD_LND_TLS_CERT_PATH="$(./scripts/devnet paths alice | awk -F= '/^tls_cert_path=/{print $2}')"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

### 2) Bob で go-lcpd を起動（Requester のみ / 別ターミナル）

```sh
cd go-lcpd
export LCPD_BACKEND=disabled
export LCPD_LOG_LEVEL=debug

export LCPD_LND_RPC_ADDR="$(./scripts/devnet paths bob | awk -F= '/^rpc_addr=/{print $2}')"
export LCPD_LND_TLS_CERT_PATH="$(./scripts/devnet paths bob | awk -F= '/^tls_cert_path=/{print $2}')"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50052
```

### 3) `ListLCPPeers` を呼ぶ（manifest 交換の確認）

```sh
cd go-lcpd
./bin/lcpdctl lcpd list-lcp-peers -s 127.0.0.1:50052 -o prettyjson
```

`peers[0].remoteManifest.supportedTasks[].llmChat.profile` に `gpt-5.2` が見えれば、Provider がプロファイルを広告できています。

### 4) Bob から Alice にジョブを 1 つ送る（Quote → Pay → Result）

```sh
cd go-lcpd

ALICE_PUBKEY="$(./scripts/devnet lncli alice getinfo | jq -r .identity_pubkey)"

./bin/lcpd-oneshot \
  -server-addr 127.0.0.1:50052 \
  -peer-id "$ALICE_PUBKEY" \
  -pay-invoice \
  -profile gpt-5.2 \
  -prompt "Say hello in one word." \
  -timeout 60s
```

## ログ / データの場所

- state/log: `./.data/devnet/`
- bitcoind log: `./.data/devnet/logs/bitcoind.log`
- lnd log: `./.data/devnet/logs/lnd-alice.log` / `./.data/devnet/logs/lnd-bob.log`

最初から作り直す（破壊的）:

```sh
./scripts/devnet down
rm -rf ./.data/devnet
```

