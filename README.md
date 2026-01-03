# LCP (Lightning Compute Protocol)

LCP (Lightning Compute Protocol) is an application-layer protocol for paying for small compute jobs over Lightning.
It uses BOLT #1 custom messages on a direct Lightning peer connection.

LCP v0.2 defines a quote → pay → stream flow:
- Both sides exchange `lcp_manifest` (mandatory) to advertise limits.
- The Requester sends `lcp_quote_request`, then streams the input (`lcp_stream_begin/chunk/end` with `stream_kind=input`).
- The Provider replies with `lcp_quote_response` containing `price_msat`, `terms_hash`, and a BOLT11 invoice.
- After settlement, the Provider streams the result (`stream_kind=result`) and then sends `lcp_result` (terminal completion + metadata).

LCP binds payment to job terms by setting the invoice `description_hash` to `terms_hash`.

Limitations:
- Payloads are limited by the BOLT #1 custom message size (about 65 KB), but v0.2 supports large inputs/results via chunked streams bounded by peer-declared limits (`max_payload_bytes`, `max_stream_bytes`, `max_job_bytes`).
- The Requester and Provider must be directly peered. This leaks metadata compared to onion messages or blinded paths.
- Payment happens before execution. This is not an atomic swap (and v0.2 also requires sending the full input stream before quoting).

Reference: BOLT #1 messaging (`https://github.com/lightning/bolts/blob/master/01-messaging.md`).

## One-shot client demo

The one-shot client in [go-lcpd/tools/lcpd-oneshot](go-lcpd/tools/lcpd-oneshot) provides a simple way to execute a task in a single request using the LCP protocol.
It serves as a reference implementation demonstrating how to use the protocol.

![demo](go-lcpd/tools/lcpd-oneshot/demo.gif)

## Potential future work

These are design ideas. They are not implemented in this repo:
- Large payload delivery: optional encrypted out-of-band transport or compression on top of streams.
- Privacy improvements: BOLT 12 offers and blinded paths to reduce direct peering requirements.
- Integration layers: Lightning Service Provider (LSP) integration (an OpenAI-compatible gateway lives in `apps/openai-serve/`).

## Safety / use at your own risk

This project is unaudited. Running it against real funds and real peers can lead to loss of funds.

- Sending funds / opening channels / paying invoices on mainnet can lead to loss of funds.
- Never leak secrets such as seed phrases, wallet passwords, macaroons, or API keys.
- Start with [docs/go-lcpd/docs/regtest.md](docs/go-lcpd/docs/regtest.md) first (free and safer), then try mainnet with small amounts.
- `go-lcpd` integrates with `lnd` (gRPC) for peer messaging and payments. Other Lightning implementations are not supported by this repo.
  This repository does not ship an `lnd/` folder or binaries — bring your own `lnd`.
- LCP runs over direct Lightning peer connections (BOLT #1 custom messages). You must be peered with the Provider, and you may need a channel/route to pay.

## Start here

- Overview: this README
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)
- Quickstart (mainnet): [docs/go-lcpd/docs/quickstart-mainnet.md](docs/go-lcpd/docs/quickstart-mainnet.md)
- Configuration: [docs/go-lcpd/docs/configuration.md](docs/go-lcpd/docs/configuration.md)
- Background run + logging: [docs/go-lcpd/docs/background.md](docs/go-lcpd/docs/background.md)
- regtest walkthrough: [docs/go-lcpd/docs/regtest.md](docs/go-lcpd/docs/regtest.md)
- Protocol spec (LCP v0.2): [docs/protocol/protocol.md](docs/protocol/protocol.md)
- One-shot client (demo): [go-lcpd/tools/lcpd-oneshot](go-lcpd/tools/lcpd-oneshot)

## Docs site (Mintlify)

This repository’s docs site is managed with Mintlify (`docs/docs.json`).
All Mintlify pages and assets live under `docs/` (Japanese pages are colocated with English and use a `-ja` suffix).

Local preview:

```sh
cd docs
npx --yes mintlify@4.2.255 dev --no-open
```

Validate navigation + check docs quality:

```sh
cd docs
node scripts/check-docs-json.mjs
npx --yes mintlify@4.2.255 a11y
npx --yes mintlify@4.2.255 broken-links
```

## Repository layout (high level)

- `docs/protocol/`: the LCP wire protocol spec (BOLT-style TLV + state machine)
- `proto-go/`: shared Go protobuf/gRPC types for the LCPD API
- `go-lcpd/`: reference implementation (Lightning Compute Protocol Daemon)
- `apps/openai-serve/`: OpenAI-compatible HTTP gateway (forwards to `lcpd-grpcd` over gRPC)

## Go workspace (go.work.sum)

This repo is a multi-module Go repository (each subproject has its own `go.mod`).

If you use Go workspaces locally (`go work`), Go will generate:

- `go.work` (the workspace definition)
- `go.work.sum` (workspace checksums, similar to `go.sum`)

In this repo, these workspace files are treated as local dev artifacts and are ignored by git.
If you see a `go.work.sum` appear locally, it is safe to delete; it will be regenerated as needed.

Note: a single workspace that includes both `go-lcpd/` and `apps/openai-serve/` can hit an
`ambiguous import: google.golang.org/genproto...` error because `protoc-gen-cobra` pulls the legacy
`google.golang.org/genproto` module while gRPC uses the split `google.golang.org/genproto/googleapis/...` modules.
If you run into this, prefer running Go commands from each module directory without a workspace.

## Development (go-lcpd)

```sh
cd go-lcpd
go test ./...
```

Details: [go-lcpd/README.md](go-lcpd/README.md)
