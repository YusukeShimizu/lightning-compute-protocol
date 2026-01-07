# Quickstart (mainnet)

If you want a guided CLI that also brings up `openai-serve` (OpenAI-compatible HTTP), see
[lcp-quickstart](/lcp-quickstart/overview). This page documents the manual `go-lcpd` workflow.

## Safety & operational constraints (mainnet)

- This project is unaudited. Sending funds / opening channels / paying invoices on mainnet can lead to loss of funds.
- Start with `regtest.md` first (free/safer), and only try mainnet with small amounts.
- You are responsible for all on-chain and Lightning fees, routing failures, and liquidity management.

## Goal

Run your own `lnd`, connect to the Provider below, and complete Quote → Pay → Stream:

- Provider node: `03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309`

## Prerequisites

- Linux or macOS
- A working mainnet `lnd` installation (outside the scope of this repo) with `lncli` available
- Go 1.24.4+
- `jq` (optional, used only for pretty-printing JSON in some steps)

Note: This repository does not ship an `lnd/` folder or binaries — bring your own `lnd`.

## 0) Build `go-lcpd` CLI tools (one-time)

This quickstart avoids Nix and installs binaries into `go-lcpd/bin/`:

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

All commands below assume you run `./bin/lcpd-grpcd`, `./bin/lcpdctl`, and `./bin/lcpd-oneshot`.

## 1) Start lnd (mainnet)

Start `lnd` in mainnet mode using your preferred setup. You will need:

- lnd gRPC address (e.g., `localhost:10009`)
- TLS cert path (e.g., `~/.lnd/tls.cert`)
- Admin macaroon path (e.g., `~/.lnd/data/chain/bitcoin/mainnet/admin.macaroon`)

## 2) Create/unlock wallet (first time)

First time (interactive):

```sh
lncli create
```

If unlock is needed after restart (interactive):

```sh
lncli unlock
```

## 3) Connect to the Provider (Lightning peer connect)

```sh
PROVIDER_NODE="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309"
lncli connect "$PROVIDER_NODE"
lncli listpeers
```

## 4) Prepare for payments (funds + channel)

To use `lcpd-oneshot -pay-invoice`, your node must be able to pay:

- it has on-chain funds
- it has at least one channel with outbound liquidity (the shortest path is a direct channel to the Provider)

Example (illustrative only; amounts/confirmations are your responsibility):

```sh
PROVIDER_PUBKEY="03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983"

# create a deposit address, fund it, and wait for confirmations
lncli newaddress p2wkh
lncli walletbalance

# open a channel to the Provider (may fail depending on Provider policy)
lncli openchannel --node_key "$PROVIDER_PUBKEY" --local_amt 20000
lncli listchannels
```

## 5) Start go-lcpd (Requester)

```sh
cd go-lcpd

export LCPD_BACKEND=disabled
export LCPD_LOG_LEVEL=debug

export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

## 6) Inspect Provider supported models (optional)

In another terminal:

```sh
cd go-lcpd
./bin/lcpdctl lcpd list-lcp-peers -s 127.0.0.1:50051 -o prettyjson
```

`peers[].remoteManifest.supportedTasks[].openaiChatCompletionsV1.model` contains the Provider models (if advertised).

Notes:

- `supportedTasks` is an optional field in `lcp_manifest`. If the Provider does not configure `llm.models` (or Provider mode is disabled), it will not advertise `supported_tasks`.
- `-o prettyjson` omits empty fields, so it may not show `supportedTasks` even if the field is absent by design.

## 7) Run one job (Quote → Pay → Stream)

In another terminal:

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

## 8) (Optional) Start an interactive chat session

This keeps a local transcript and sends it as part of each new prompt. Each turn prints the invoice amount and a running total:

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

## Troubleshooting

- `peer is not ready for lcp`: check that `lnd` is connected to the Provider (`lncli listpeers`), and confirm `lcpdctl list-lcp-peers` sees it.
- `lcp_error code=2: unsupported model ...`: set `-model` to a value the Provider supports (see step 6).
- `payment failed`: you may have no channel / no route / insufficient liquidity. Check `lncli walletbalance` and `lncli listchannels`.
