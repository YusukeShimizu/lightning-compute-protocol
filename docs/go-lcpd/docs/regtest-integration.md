# regtest integration test (bitcoind + lnd)

This document explains how to run `TestIntegration_Regtest_LNDPayment` locally (a regtest integration test for Lightning payments).

## Prerequisites

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` on `PATH`
- `lnd` / `lncli` on `PATH`

## Run

```sh
cd go-lcpd
export LCP_ITEST_REGTEST=1

# main test (invoice creation + payment)
go test ./itest/e2e -run Regtest_LNDPayment -count=1 -v

# optional coverage
go test ./itest/e2e -run Regtest_LNDCustomMessages -count=1 -v
go test ./itest/e2e -run RequesterGRPC -count=1 -v
go test ./itest/e2e -run Regtest -count=1 -v
```

## devnet (manual)

If you want to run a manual regtest devnet (bitcoind + 2x lnd) instead of integration tests, use `./scripts/devnet`.

Steps: `devnet.md`

### custom messages (`lcp_manifest` exchange)

`TestIntegration_Regtest_LNDCustomMessages_LCPManifest` is an integration test showing that two `lnd` nodes can exchange `lcp_manifest` over BOLT #1 custom messages, and that `ListLCPPeers` can observe one peer (no channel required).
Run it using the "optional coverage" commands above.

## Generated data/logs

The tests store `bitcoind` and `lnd` state/logs under a temp directory created with `os.MkdirTemp`.
Normally it is removed at the end of the test, except when:

- `LCP_ITEST_KEEP_DATA=1` is set
- the test fails

On failure (or when keeping data), the test log prints `keeping regtest data dir: ...`.

## Implementation notes (stability knobs)

- `bitcoind` starts with `-listen=0` (no P2P listen; RPC only).
- `lnd` enables `--bitcoind.rpcpolling` to avoid ZMQ port conflicts.

## About the lnd gRPC stubs

This project does not import `github.com/lightningnetwork/lnd/lnrpc` directly.
Instead it uses vendored Go stubs under `go-lcpd/internal/lnd/lnrpc` and `go-lcpd/internal/lnd/routerrpc`.
This avoids dependency conflicts caused by protobuf replacement/forks, and keeps `go test` straightforward.

To update, copy/replace the stubs from the target lnd module version:

```sh
cd go-lcpd
./scripts/update-lnd-stubs v0.19.3-beta
```
