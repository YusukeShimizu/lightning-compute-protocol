# Regtest smoke test (2 nodes)

This document describes a reproducible regtest smoke test for `apps/ldk-lcp-node/`:

- start 2 nodes (Alice/Bob)
- connect peers
- open a channel
- create an invoice
- pay the invoice (direct channel)

## Prerequisites

- Nix (flakes enabled)
- Docker daemon running (Nigiri uses Docker under the hood)

This guide assumes a Nix-first workflow. Enter the devShell from the repo root:

```sh
nix develop .#ldk-lcp-node
```

The devShell provides: Rust toolchain (`cargo`), `protoc`, `grpcurl`, `jq`, `python3`, `curl`, and `nigiri`.

Optional (direnv): run `direnv allow` in `apps/ldk-lcp-node/` to auto-enter the same devShell on `cd`.

Start the regtest stack (requires Docker daemon running):

```sh
nigiri start
```

Nigiri exposes an Esplora-compatible API via Chopsticks on `http://127.0.0.1:3000`.

## Run the smoke test script

```sh
cd apps/ldk-lcp-node

# optional: override if you are not using nigiri/chopsticks
export ESPLORA_BASE_URL="http://127.0.0.1:3000"

bash scripts/regtest-smoke.sh
```

The script writes logs under:

- `apps/ldk-lcp-node/dev-data/regtest-smoke/alice.log`
- `apps/ldk-lcp-node/dev-data/regtest-smoke/bob.log`

## Manual operation (grpcurl snippets)

If you want to run the steps manually, use the `lnnode.v1` proto under `go-lcpd/proto/`:

```sh
PROTO_ROOT=go-lcpd/proto
PROTO=$PROTO_ROOT/lnnode/v1/lnnode.proto

grpcurl -plaintext -import-path "$PROTO_ROOT" -proto "$PROTO" 127.0.0.1:10010 \
  lnnode.v1.LightningNodeService/GetNodeInfo
```
