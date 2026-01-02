# Try on regtest (safer / free)

Before mainnet, use `regtest` to try the Quote → Pay → Stream flow.

There are two routes:

- A) Automated (integration tests): tests auto-start `bitcoind` (regtest) + 2x `lnd`
- B) Manual (devnet): run `./scripts/devnet` locally and operate it by hand

## Prerequisites (no Nix)

- Go 1.24.4+
- `bitcoind` / `bitcoin-cli` on `PATH`
- `lnd` / `lncli` on `PATH`

## A) Automated (integration tests)

```sh
cd go-lcpd

LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_LNDPayment -count=1 -v
LCP_ITEST_REGTEST=1 go test ./itest/e2e -run Regtest_LNDCustomMessages -count=1 -v
LCP_ITEST_REGTEST=1 go test ./itest/e2e -run RequesterGRPC -count=1 -v
```

Details: `regtest-integration.md`

## B) Manual (devnet)

### 1) Start devnet (bitcoind + 2x lnd)

```sh
cd go-lcpd
./scripts/devnet up
./scripts/devnet status
```

First time only (interactive):

```sh
./scripts/devnet lncli alice create
./scripts/devnet lncli bob create
```

### 2) Funding / channel / smoke checks

The manual steps are longer; see `devnet.md`.

### 3) Start go-lcpd → run oneshot

For Provider configuration (YAML) and profiles, see `configuration.md`.

## Data/logs and cleanup (destructive)

- devnet state/log: `./.data/devnet/`

To rebuild from scratch (destructive):

```sh
cd go-lcpd
./scripts/devnet down
rm -rf ./.data/devnet
```
