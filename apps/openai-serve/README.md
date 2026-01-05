# openai-serve (OpenAI-compatible LCP gateway)

`openai-serve` is a small OpenAI-compatible HTTP server that forwards requests to a running `lcpd-grpcd` (Requester) over gRPC.

Supported endpoints:

- `POST /v1/chat/completions` (JSON or `stream:true` SSE passthrough)
- `POST /v1/responses` (JSON or `stream:true` SSE passthrough)
- `GET /v1/models`
- `GET /healthz`

## Build

```sh
cd apps/openai-serve
go install ./cmd/openai-serve
```

## Run

Start `lcpd-grpcd` separately (Requester mode) and ensure it can reach your Lightning node and LCP peers.

Then:

```sh
export OPENAI_SERVE_HTTP_ADDR="127.0.0.1:8080"
export OPENAI_SERVE_LCPD_GRPC_ADDR="127.0.0.1:50051"

# Optional auth (comma-separated)
export OPENAI_SERVE_API_KEYS="devkey1"

# Optional routing
# export OPENAI_SERVE_DEFAULT_PEER_ID="02ab...66hex..."
# export OPENAI_SERVE_MODEL_MAP="gpt-5.2=02ab...;gpt-4.1-mini=03cd..."

openai-serve
```

## Examples (curl)

Chat Completions (non-streaming):

```sh
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer devkey1' \
  -d '{"model":"gpt-5.2","messages":[{"role":"user","content":"Say hello."}]}'
```

Chat Completions (streaming):

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer devkey1' \
  -d '{"model":"gpt-5.2","stream":true,"messages":[{"role":"user","content":"Say hello."}]}'
```

Responses (streaming):

```sh
curl -N http://127.0.0.1:8080/v1/responses \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer devkey1' \
  -d '{"model":"gpt-5.2","stream":true,"input":"Say hello."}'
```

## Environment variables

| Name | Default | Notes |
| --- | --- | --- |
| `OPENAI_SERVE_HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen `host:port` |
| `OPENAI_SERVE_LCPD_GRPC_ADDR` | `127.0.0.1:50051` | `lcpd-grpcd` gRPC address |
| `OPENAI_SERVE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `OPENAI_SERVE_API_KEYS` | (empty) | If set, require `Authorization: Bearer ...` |
| `OPENAI_SERVE_DEFAULT_PEER_ID` | (empty) | Default LCP peer id (66 hex chars) |
| `OPENAI_SERVE_MODEL_MAP` | (empty) | `model=peer_id;model2=peer_id` |
| `OPENAI_SERVE_MODEL_ALLOWLIST` | (empty) | Comma-separated model IDs |
| `OPENAI_SERVE_ALLOW_UNLISTED_MODELS` | `false` | If `true`, skip model validation |
| `OPENAI_SERVE_MAX_PRICE_MSAT` | `0` | If >0, reject quotes exceeding this |
| `OPENAI_SERVE_TIMEOUT_QUOTE` | `5s` | gRPC quote timeout |
| `OPENAI_SERVE_TIMEOUT_EXECUTE` | `120s` | gRPC execute timeout |

## How it works

High-level data flow:

1. Your client calls the OpenAI-compatible HTTP endpoint (`/v1/chat/completions` or `/v1/responses`).
2. `openai-serve` forwards the raw request body bytes as an LCP `openai.chat_completions.v1` or
   `openai.responses.v1` task.
3. `openai-serve` forwards the task to a local `lcpd-grpcd` (Requester) over gRPC.
4. The Requester talks to an LCP Provider over Lightning custom messages, requests a quote, pays, and receives the result.
5. `openai-serve` returns the raw Provider response bytes (no re-encoding) and includes LCP metadata in response headers.

The Requester (`lcpd-grpcd`) is the component that owns your Lightning node connection and can spend sats.
`openai-serve` is a stateless HTTP gateway; it does not connect to Lightning directly.

## Request support and constraints

### Supported endpoints

- `POST /v1/chat/completions` (JSON or `stream:true` SSE passthrough)
- `POST /v1/responses` (JSON or `stream:true` SSE passthrough)
- `GET /v1/models`
- `GET /healthz`

### Passthrough behavior

- Requests are forwarded byte-for-byte to Providers; unknown fields are accepted and forwarded.
- `stream:true` passes through `text/event-stream` bytes as they arrive. Without `stream` (or `false`), the full JSON
  response body is returned.
- Minimal validation before routing:
  - Body must be valid JSON.
  - `model` must be present, non-empty, and must not have leading/trailing whitespace.
  - `messages` (chat completions) or `input` (responses) must be present and non-empty.
  - HTTP `Content-Encoding` must be empty or `identity` (compressed request bodies are rejected).
- Request body is limited to 1 MiB.
- Provider result bytes are returned as-is. HTTP `Content-Type`/`Content-Encoding` are taken from the LCP result metadata.

## Routing and model selection

### Peer selection order

For a given `model`, the peer is chosen in this order:

1. `OPENAI_SERVE_MODEL_MAP` (`model=peer_id;...`) if the peer is connected/LCP-ready.
2. `OPENAI_SERVE_DEFAULT_PEER_ID` if set and connected/LCP-ready.
3. A peer that advertises the model in `supported_tasks` (if any).
4. Fallback to the first connected peer.

If there are no connected peers, the request fails.

### Model validation

- If `OPENAI_SERVE_MODEL_ALLOWLIST` is set: `model` must be in the allowlist (unless `OPENAI_SERVE_ALLOW_UNLISTED_MODELS=true`).
- Otherwise: if any connected peers advertise `supported_tasks`, the model must be advertised by at least one peer.
  If no peers advertise `supported_tasks`, validation is skipped to keep the gateway usable.

## Safety knobs

- `OPENAI_SERVE_MAX_PRICE_MSAT`: reject quotes above a maximum price (helps prevent accidental overspend).
- `OPENAI_SERVE_TIMEOUT_QUOTE` / `OPENAI_SERVE_TIMEOUT_EXECUTE`: bound quote/execution time.
- `OPENAI_SERVE_API_KEYS`: simple API-key auth for the HTTP gateway.

## Logging and privacy

This service treats logs as sensitive.

- Logs MUST NOT contain raw prompts (`messages[].content`) or raw model outputs.
- Logs include only operational metadata (e.g., peer id, job id, price, durations, and byte/token counts).
- `OPENAI_SERVE_LOG_LEVEL=debug` enables more verbose request logging; keep `info` (default) for production unless needed.

## Response metadata headers

`POST /v1/chat/completions` and `POST /v1/responses` include LCP metadata headers:

- `X-Lcp-Peer-Id`: the chosen Provider peer id
- `X-Lcp-Job-Id`: the job id (hex)
- `X-Lcp-Price-Msat`: the accepted quote price
- `X-Lcp-Terms-Hash`: the accepted quote terms hash (hex)

## Development

```sh
cd apps/openai-serve
make test
make lint
make fmt
```

This repo expects `golangci-lint v2` installed locally.
