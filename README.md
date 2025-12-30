# LCP (Lightning Compute Protocol)

LCP (Lightning Compute Protocol) is an application-layer protocol for paying for small compute jobs over Lightning.
It uses BOLT #1 custom messages on a direct Lightning peer connection.

LCP defines a quote → pay → result flow:
- The Requester sends `lcp_quote_request`.
- The Provider replies with `lcp_quote_response` containing `price_msat`, `terms_hash`, and a BOLT11 invoice.
- After settlement, the Provider sends `lcp_result`.

LCP binds payment to job terms by setting the invoice `description_hash` to `terms_hash`.

Limitations:
- Payloads are limited by BOLT #1 custom message size (about 65 KB). LCP results must fit in one message.
- The Requester and Provider must be directly peered. This leaks metadata compared to onion messages or blinded paths.
- Payment happens before execution. This is not an atomic swap.

Reference: BOLT #1 messaging (`https://github.com/lightning/bolts/blob/master/01-messaging.md`).

## One-shot client demo

The one-shot client in [go-lcpd/tools/lcpd-oneshot](go-lcpd/tools/lcpd-oneshot) provides a simple way to execute a task in a single request using the LCP protocol.
It serves as a reference implementation demonstrating how to use the protocol.

![demo](go-lcpd/tools/lcpd-oneshot/demo.gif)

## Potential future work

These are design ideas. They are not implemented in this repo:
- Large payload delivery: chunking or optional encrypted out-of-band transport.
- Privacy improvements: BOLT 12 offers and blinded paths to reduce direct peering requirements.
- Integration layers: OpenAI-compatible middleware and Lightning Service Provider (LSP) integration.

## Safety / use at your own risk

This project is unaudited. Running it against real funds and real peers can lead to loss of funds.

- Sending funds / opening channels / paying invoices on mainnet can lead to loss of funds.
- Never leak secrets such as seed phrases, wallet passwords, macaroons, or API keys.
- Start with [go-lcpd/docs/regtest.md](go-lcpd/docs/regtest.md) first (free and safer), then try mainnet with small amounts.
- `go-lcpd` integrates with `lnd` (gRPC) for peer messaging and payments. Other Lightning implementations are not supported by this repo.
  This repository does not ship an `lnd/` folder or binaries — bring your own `lnd`.
- LCP runs over direct Lightning peer connections (BOLT #1 custom messages). You must be peered with the Provider, and you may need a channel/route to pay.

## Start here

- Overview: this README
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)
- Quickstart (mainnet): [go-lcpd/docs/quickstart-mainnet.md](go-lcpd/docs/quickstart-mainnet.md)
- Configuration: [go-lcpd/docs/configuration.md](go-lcpd/docs/configuration.md)
- Background run + logging: [go-lcpd/docs/background.md](go-lcpd/docs/background.md)
- regtest walkthrough: [go-lcpd/docs/regtest.md](go-lcpd/docs/regtest.md)
- Protocol spec (LCP v0.1): [protocol/protocol.md](protocol/protocol.md)
- One-shot client (demo): [go-lcpd/tools/lcpd-oneshot](go-lcpd/tools/lcpd-oneshot)

## Repository layout (high level)

- `protocol/`: the LCP wire protocol spec (BOLT-style TLV + state machine)
- `go-lcpd/`: reference implementation (Lightning Compute Protocol Daemon)

## Development (go-lcpd)

```sh
cd go-lcpd
go test ./...
```

Details: [go-lcpd/README.md](go-lcpd/README.md)
