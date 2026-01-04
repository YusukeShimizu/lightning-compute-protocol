# CLI (lcpdctl) — protoc-gen-cobra client for go-lcpd

This document explains how to use:

- `lcpdctl`: a generated CLI client for the `go-lcpd` gRPC API (`lcpd.v1.LCPDService`)
- `lcpd-oneshot`: a small helper that runs `RequestQuote` and (optionally) `AcceptAndExecute`

## Prerequisites

- Go 1.24.4+
- `jq` (optional, used only for pretty-printing JSON in some examples)

## Install the CLI tools (recommended)

Install binaries into `go-lcpd/bin/` (no Nix required):

```sh
cd go-lcpd
mkdir -p bin
GOBIN="$PWD/bin" go install ./tools/lcpd-grpcd ./tools/lcpdctl ./tools/lcpd-oneshot
```

All command examples below use:
- `./bin/lcpd-grpcd`
- `./bin/lcpdctl`
- `./bin/lcpd-oneshot`

## Security & operational constraints

- `lcpd-grpcd` serves plaintext gRPC (no TLS, no auth). Bind to `127.0.0.1` or protect it via SSH/VPN/reverse-proxy if you must access it remotely.
- `AcceptAndExecute` pays a BOLT11 invoice via your `lnd`. On mainnet this spends real funds. Start with regtest and use small amounts.
- LCP messaging requires an active Lightning peer connection to the Provider, and the Provider must support LCP (manifest observed).
- The only supported task type is `openai.chat_completions.v1` (raw OpenAI-compatible request/response JSON bytes passthrough).
- Quote execution is time-bounded by `terms.quoteExpiry`; calls after expiry are expected to fail.
- LCP peer messaging enforces payload/stream limits (`max_payload_bytes`, `max_stream_bytes`, `max_job_bytes` in the manifest). `go-lcpd` defaults to `16384` bytes payload, `4 MiB` stream, `8 MiB` job.
- `job_id` CLI flags are base64 (proto `bytes`), matching protojson encoding.
- `lcpdctl --timeout` is a dial timeout (not an RPC deadline). Cancel long-running RPCs with Ctrl-C, or use `lcpd-oneshot -timeout ...` for an overall deadline.

Note about older docs:
- Some older docs used step names like `CreateQuote → Execute → VerifyReceipt`.
  In `go-lcpd` today, the public gRPC flow is `RequestQuote → AcceptAndExecute`, and invoice binding is verified inside `AcceptAndExecute`.

## Run (gRPC daemon)

Start the gRPC daemon in another terminal.

### Requester mode (recommended default)

Requester mode needs `lnd` connectivity for peer messaging and invoice payments:

```sh
cd go-lcpd

export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"

./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

### Provider mode (optional)

Provider mode requires:
- `LCPD_PROVIDER_CONFIG_PATH` (YAML)
- `LCPD_BACKEND` (e.g. `openai`)
- `LCPD_LND_*` (because the Provider also uses peer messaging)

Details: `docs/configuration.md`.

### lnd disabled (gRPC only)

```sh
cd go-lcpd
./bin/lcpd-grpcd -grpc_addr=127.0.0.1:50051
```

This starts the daemon, but LCP peer operations (`ListLCPPeers`, `RequestQuote`, `AcceptAndExecute`) require `lnd`.

## One-shot CLI (`lcpd-oneshot`)

`lcpd-oneshot` is a small wrapper over the gRPC API:
- always runs `RequestQuote`
- runs `AcceptAndExecute` only when `-pay-invoice=true`

Defaults:
- `server-addr=127.0.0.1:50051`
- `model=gpt-5.2`
- `timeout=30s`

Constraints:
- `lcpd-oneshot` uses insecure gRPC (no TLS). Use it with a localhost-bound `lcpd-grpcd`.
- `peer-id` is required (66-hex compressed pubkey).

Prompt selection priority: `--prompt` → positional args (space-joined) → stdin.

Example (text output):

```sh
cd go-lcpd
./bin/lcpd-oneshot \
  -peer-id "<provider_pubkey_hex>" \
  -model gpt-5.2 \
  -prompt "Say hello in one word."
```

Use `--json` if you want JSON output.

Note on pricing output:
- The tool prints both `price_msat` and a convenience `price_sat` (decimal string, milli-sat precision).

### Interactive chat mode (`-chat`)

`-chat` runs a simple interactive REPL that keeps conversation context by embedding the full transcript into each new prompt.
Each turn prints the invoice amount and a running total:

- `paid=<sat> sat total=<sat> sat`

Usage:

```sh
cd go-lcpd
./bin/lcpd-oneshot \
  -peer-id "<provider_pubkey_hex>" \
  -model gpt-5.2 \
  -pay-invoice \
  -chat
```

Notes:
- `-chat` requires `-pay-invoice=true` (otherwise you can't see model responses).
- Type `/exit` (or Ctrl-D) to quit.
- When stdin/stdout are terminals, `lcpd-oneshot` uses a small Bubble Tea TUI for readability; otherwise it falls back to the plain REPL.
- Chat history may be truncated to fit `-max-prompt-bytes` (default: `12000`). Set `-max-prompt-bytes=0` to disable trimming (may hit payload limits).
- Optional: `-system-prompt "..."` to prefix the conversation with a System instruction.

## lnd integration (custom messages / `lcp_manifest` exchange)

When configured, `go-lcpd` connects to `lnd` over gRPC and exchanges `lcp_manifest` over BOLT #1 custom messages.
This enables automatic discovery of LCP-capable peers on top of an existing Lightning peer connection.
You can list them via `ListLCPPeers`.

### Required configuration (choose one)

#### A) Environment variables (recommended)

```sh
# lnd gRPC
export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"

# optional: macaroons (mainnet/testnet, etc.)
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"

# Optional: periodically re-send `lcp_manifest` to connected peers (unset or "0s" disables).
# export LCPD_LND_MANIFEST_RESEND_INTERVAL="10s"
```

Notes:
- On regtest with `lnd --no-macaroons`, a macaroon is not required (you can leave it unset).
- If `LCPD_LND_RPC_ADDR` is unset, lnd integration is disabled.
- If `LCPD_LND_TLS_CERT_PATH` is unset (or empty), TLS verification uses system roots. This works when lnd presents a publicly trusted certificate. For self-signed certs, set `LCPD_LND_TLS_CERT_PATH`.
- Use `LCPD_LOG_LEVEL=debug` for more verbose logs around `ListLCPPeers` and related flows.

#### B) `lcpd-grpcd` flags

If you prefer not to use env vars, you can pass these flags to `lcpd-grpcd`:

```sh
./bin/lcpd-grpcd \
  -grpc_addr=127.0.0.1:50051 \
  -lnd_rpc_addr="localhost:10009" \
  -lnd_tls_cert_path="$HOME/.lnd/tls.cert" \
  -lnd_admin_macaroon_path="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"
```

### Smoke check (`ListLCPPeers`)

1) Connect to the remote as an `lnd` peer (e.g., `lncli connect <pubkey>@<host:port>`)
2) Run `go-lcpd` on both sides (each connected to its own `lnd`)
3) Call `ListLCPPeers`

```sh
cd go-lcpd
./bin/lcpdctl lcpd list-lcp-peers -s 127.0.0.1:50051 -o prettyjson
```

Tip:
- Right after lnd starts / wallet unlock, `SubscribePeerEvents` / `SubscribeCustomMessages` may take time to stabilize.
  If needed, increase the timeout via `-startup_timeout=60s` (for example).

## Quickstart (RequestQuote → AcceptAndExecute)

This section shows the full flow using only `lcpdctl lcpd ...` (quote → pay → stream(result) → `lcp_result`).

- The examples do not write files (stdout only).
- No Python is needed (we use `jq` to format JSON).
- `jq` is optional (used only for pretty-printing).

### end-to-end (RequestQuote → AcceptAndExecute)

This flow assumes:
- `lcpd-grpcd` is configured with `lnd` (`LCPD_LND_*`)
- the Provider is connected as an `lnd` peer

#### 1) RequestQuote (get `terms` + invoice)

```sh
cd go-lcpd

SERVER_ADDR="127.0.0.1:50051"
PEER_ID="<provider_pubkey_hex>"
MODEL="gpt-5.2"

PROMPT="Say hello in one word."

request_json="$(jq -nc --arg model "$MODEL" --arg prompt "$PROMPT" \
  '{model:$model, messages:[{role:"user", content:$prompt}]}' )"
request_json_b64="$(printf '%s' "$request_json" | base64 | tr -d '\n')"

quote_json="$(./bin/lcpdctl lcpd request-quote \
  -s "$SERVER_ADDR" \
  --peer-id "$PEER_ID" \
  --task-openai-chat-completions-v1 \
  --task-openai-chat-completions-v1-params-model "$MODEL" \
  --task-openai-chat-completions-v1-request-json "$request_json_b64" \
  -o json)"
echo "$quote_json" | jq -C .
# If you don't have `jq` installed, use: echo "$quote_json"
```

#### 2) AcceptAndExecute (pay invoice and wait for result)

`jobId` is base64 in protojson (because `job_id` is `bytes`).

```sh
cd go-lcpd

job_id_b64="$(echo "$quote_json" | jq -r '.terms.jobId')"

exec_json="$(./bin/lcpdctl lcpd accept-and-execute \
  -s "$SERVER_ADDR" \
  --peer-id "$PEER_ID" \
  --job-id "$job_id_b64" \
  --pay-invoice \
  -o json)"
echo "$exec_json" | jq -C .
# If you don't have `jq` installed, use: echo "$exec_json"
```

Note: `AcceptAndExecuteResponse.result.result` is base64-encoded bytes in JSON output.
To decode and pretty-print the raw OpenAI response JSON:

```sh
result_b64="$(echo "$exec_json" | jq -r '.result.result')"

# GNU coreutils:
# echo "$result_b64" | base64 -d | jq -C .
#
# macOS:
# echo "$result_b64" | base64 -D | jq -C .
```
