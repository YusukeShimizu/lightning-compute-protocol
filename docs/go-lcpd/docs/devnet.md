# regtest devnet (bitcoind + 2x lnd) manual runbook

Use this doc to run a local regtest devnet (Bitcoin Core + two `lnd` nodes).
It exercises `go-lcpd` Lightning integration (custom messages, invoice binding, payments).

This devnet is managed by `./scripts/devnet`.
It stores all state/logs under `./.data/devnet/` (gitignored).

If a step fails, check logs/state under `./.data/devnet/`.
Re-run with `LCPD_LOG_LEVEL=debug`.

This doc runs a minimal Quote → Pay → Result flow with two roles:

- Alice: Provider. Configure Provider mode only on Alice. Use `LCPD_BACKEND=deterministic` to avoid external APIs.
- Bob: Requester.

## Prerequisites

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` on `PATH`
- `lnd` / `lncli` on `PATH`
- `jq` on `PATH` (used in the commands below)

## Build go-lcpd CLI tools (one-time)

Install binaries into `go-lcpd/bin/`:

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

## Start / stop

Start:

```sh
cd go-lcpd
./scripts/devnet up
```

Status:

```sh
./scripts/devnet status
./scripts/devnet info
```

Stop:

```sh
./scripts/devnet down
```

## Wallet init / unlock (first time)

`lnd` can start without an initialized wallet, but you must create a wallet before using RPCs (payments/channels/invoices).

First time (interactive):

```sh
./scripts/devnet lncli alice create
./scripts/devnet lncli bob create
```

If unlock is required after restart (interactive):

```sh
./scripts/devnet lncli alice unlock
./scripts/devnet lncli bob unlock
```

Smoke check:

```sh
./scripts/devnet lncli alice getinfo
./scripts/devnet lncli bob getinfo
```

## Fund on-chain (make Alice the miner)

1) Create a receive address for Alice:

```sh
ADDR="$(./scripts/devnet lncli alice newaddress p2wkh | jq -r .address)"
echo "$ADDR"
```

2) Mine blocks on regtest to fund Alice (mine 101 blocks to satisfy coinbase maturity):

```sh
./scripts/devnet bitcoin-cli generatetoaddress 101 "$ADDR"
./scripts/devnet lncli alice walletbalance
```

## Connect nodes / open channel / pay (make Bob able to pay Alice)

### 1) Connect to Bob

```sh
BOB_PUBKEY="$(./scripts/devnet lncli bob getinfo | jq -r .identity_pubkey)"
BOB_P2P_ADDR="$(./scripts/devnet paths bob | awk -F= '/^p2p_addr=/{print $2}')"

./scripts/devnet lncli alice connect "${BOB_PUBKEY}@${BOB_P2P_ADDR}"
./scripts/devnet lncli alice listpeers
```

### 2) Open a channel from Alice → Bob (push funds so Bob can pay)

```sh
./scripts/devnet lncli alice openchannel --node_key "$BOB_PUBKEY" --local_amt 200000 --push_amt 10000
./scripts/devnet bitcoin-cli generatetoaddress 3 "$ADDR"
./scripts/devnet lncli alice listchannels
./scripts/devnet lncli bob listchannels

# confirm Bob has enough outbound balance to pay Alice (assumes `push_amt` shows up on Bob)
./scripts/devnet lncli bob channelbalance
```

### 3) Bob pays an invoice from Alice (connectivity/route check)

```sh
PAY_REQ="$(./scripts/devnet lncli alice addinvoice --amt 1000 | jq -r .payment_request)"
./scripts/devnet lncli bob payinvoice "$PAY_REQ"
```

## Try go-lcpd (custom messages / Quote → Pay → Result)

Once the two `lnd` nodes are connected as peers, start `go-lcpd` on both sides.
This triggers `lcp_manifest` exchange over BOLT #1 custom messages.
You can observe it via `ListLCPPeers`.

In this walkthrough, Alice runs as a Provider and returns `lcp_result` without external dependencies.
It uses `LCPD_BACKEND=deterministic`.

Provider configuration is YAML-first (`LCPD_PROVIDER_CONFIG_PATH`).

### 0) Create a Provider YAML config

Example: `go-lcpd/provider.devnet.yaml`

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

### 1) Start go-lcpd on Alice (Provider / separate terminal)

```sh
cd go-lcpd

export LCPD_BACKEND=deterministic
export LCPD_LOG_LEVEL=debug

export LCPD_PROVIDER_CONFIG_PATH="$PWD/provider.devnet.yaml"

export LCPD_LND_RPC_ADDR="$(./scripts/devnet paths alice | awk -F= '/^rpc_addr=/{print $2}')"
export LCPD_LND_TLS_CERT_PATH="$(./scripts/devnet paths alice | awk -F= '/^tls_cert_path=/{print $2}')"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

### 2) Start go-lcpd on Bob (Requester-only / separate terminal)

```sh
cd go-lcpd
export LCPD_BACKEND=disabled
export LCPD_LOG_LEVEL=debug

export LCPD_LND_RPC_ADDR="$(./scripts/devnet paths bob | awk -F= '/^rpc_addr=/{print $2}')"
export LCPD_LND_TLS_CERT_PATH="$(./scripts/devnet paths bob | awk -F= '/^tls_cert_path=/{print $2}')"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50052
```

### 3) Call `ListLCPPeers` (confirm manifest exchange)

```sh
cd go-lcpd
./bin/lcpdctl lcpd list-lcp-peers -s 127.0.0.1:50052 -o prettyjson
```

If you can see `gpt-5.2` under `peers[0].remoteManifest.supportedTasks[].llmChat.profile`, it means the Provider successfully advertised the profile.

### 4) Send one job from Bob to Alice (Quote → Pay → Result)

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

## Logs / data locations

- state/log: `./.data/devnet/`
- bitcoind log: `./.data/devnet/logs/bitcoind.log`
- lnd log: `./.data/devnet/logs/lnd-alice.log` / `./.data/devnet/logs/lnd-bob.log`

To rebuild from scratch (destructive):

```sh
./scripts/devnet down
rm -rf ./.data/devnet
```
