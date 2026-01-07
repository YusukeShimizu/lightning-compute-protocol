# lcp-quickstart

`lcp-quickstart` is a small CLI that bootstraps a local “Requester” stack:

- your local `lnd` (managed by you; optionally started by the tool)
- `lcpd-grpcd` (Requester) from `go-lcpd/`
- `openai-serve` (OpenAI-compatible HTTP) from `apps/openai-serve/`

It targets only `testnet` and `mainnet`.

## Build / Install

```sh
cd apps/lcp-quickstart
go install ./cmd/lcp-quickstart
```

## Prerequisites

- `lcp-quickstart` will auto-install `lnd` + `lncli` (downloaded into `~/.lcp-quickstart/bin/`) if they are missing.
- If you prefer a system-installed `lncli`, pass `--lncli <path>`.
- Wallet creation/unlock is **not** automated (seed safety). The tool will tell you when to run:
  - `lcp-quickstart <testnet|mainnet> lncli create`
  - `lcp-quickstart <testnet|mainnet> lncli unlock`

## Run (testnet)

`testnet` requires an explicit Provider:

```sh
lcp-quickstart testnet up --provider "<pubkey>@<host:port>"
```

If you need to open a channel (you see a “channel/route required” error):

```sh
lcp-quickstart testnet up \
  --provider "<pubkey>@<host:port>" \
  --open-channel --channel-sats <sats>
```

## Run (mainnet)

Mainnet is dangerous by default; you must explicitly opt in:

```sh
lcp-quickstart mainnet up --i-understand-mainnet
```

By default, mainnet uses this Provider node (override with `--provider`):

```text
03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309
```

## Status / Logs / Down / Reset

```sh
lcp-quickstart <testnet|mainnet> status
lcp-quickstart <testnet|mainnet> lncli <args...>
lcp-quickstart <testnet|mainnet> logs <lnd|lcpd-grpcd|openai-serve>
lcp-quickstart <testnet|mainnet> down
lcp-quickstart <testnet|mainnet> reset --force
```

`status` prints “next steps” guidance (unlock wallet, connect Provider, open a channel, etc).

## Verify (HTTP)

Health check:

```sh
curl -sS http://127.0.0.1:8080/healthz
```

Expected output:

```text
ok
```

Chat completions:

```sh
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer lcp-dev' \
  -d '{"model":"gpt-5.2","messages":[{"role":"user","content":"Say hello."}]}'
```

## Workspace layout

By default the app writes to `~/.lcp-quickstart/`:

- `state.json`: machine-readable state snapshot
- `logs/<component>.log`: logs (components: `lnd`, `lcpd-grpcd`, `openai-serve`)
- `pids/<component>.pid`: PID tracking (for managed processes)
- `bin/`: managed binaries (`lnd`, `lncli`, `lcpd-grpcd`, `openai-serve`)

Override the workspace location with `--home <dir>`.

## Safety notes

- `lcpd-grpcd` is always required to bind to loopback (`127.0.0.1` / `localhost`).
- `openai-serve` binds to `127.0.0.1:8080` by default.
  - If you bind it to a non-loopback address, you must pass `--i-understand-exposing-openai` **and** set `--openai-api-keys` non-empty.
- Mainnet defaults `OPENAI_SERVE_MAX_PRICE_MSAT` to a conservative cap; override with `--openai-max-price-msat`.

## Development

```sh
cd apps/lcp-quickstart
make test
make lint
make fmt
```

This repo expects `golangci-lint v2` installed locally.
