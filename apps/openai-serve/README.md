# openai-serve (OpenAI-compatible LCP gateway)

`openai-serve` is a small OpenAI-compatible HTTP server that forwards requests to a running `lcpd-grpcd` (Requester) over gRPC.

MVP support:

- `POST /v1/chat/completions` (non-streaming)
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

## Example (curl)

```sh
curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer devkey1' \
  -d '{
    "model": "gpt-5.2",
    "messages": [{"role":"user","content":"Say hello in Japanese."}]
  }'
```

## Environment variables

| Name | Default | Notes |
| --- | --- | --- |
| `OPENAI_SERVE_HTTP_ADDR` | `127.0.0.1:8080` | HTTP listen `host:port` |
| `OPENAI_SERVE_LCPD_GRPC_ADDR` | `127.0.0.1:50051` | `lcpd-grpcd` gRPC address |
| `OPENAI_SERVE_API_KEYS` | (empty) | If set, require `Authorization: Bearer ...` |
| `OPENAI_SERVE_DEFAULT_PEER_ID` | (empty) | Default LCP peer id (66 hex chars) |
| `OPENAI_SERVE_MODEL_MAP` | (empty) | `model=peer_id;model2=peer_id` |
| `OPENAI_SERVE_MODEL_ALLOWLIST` | (empty) | Comma-separated model IDs |
| `OPENAI_SERVE_ALLOW_UNLISTED_MODELS` | `false` | If `true`, skip model validation |
| `OPENAI_SERVE_MAX_PRICE_MSAT` | `0` | If >0, reject quotes exceeding this |
| `OPENAI_SERVE_TIMEOUT_QUOTE` | `5s` | gRPC quote timeout |
| `OPENAI_SERVE_TIMEOUT_EXECUTE` | `120s` | gRPC execute timeout |
| `OPENAI_SERVE_MAX_PROMPT_BYTES` | `60000` | Reject oversized prompts (0 disables) |
| `OPENAI_SERVE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## How it works

High-level data flow:

1. Your client calls the OpenAI-compatible HTTP endpoint (`/v1/chat/completions`).
2. `openai-serve` converts the request into an LCP `LLMChat` task.
3. `openai-serve` forwards the task to a local `lcpd-grpcd` (Requester) over gRPC.
4. The Requester talks to an LCP Provider over Lightning custom messages, requests a quote, pays, and receives the result.
5. `openai-serve` returns an OpenAI-style JSON response and includes LCP metadata in response headers.

The Requester (`lcpd-grpcd`) is the component that owns your Lightning node connection and can spend sats.
`openai-serve` is a stateless HTTP gateway; it does not connect to Lightning directly.

## Request support and constraints

### Supported endpoints

- `POST /v1/chat/completions` (non-streaming)
- `GET /v1/models`
- `GET /healthz`

### JSON decoding is strict

Requests are decoded with `DisallowUnknownFields`.
If your client sends unknown fields (not in the request struct), the request will be rejected with a 400.

### Supported request fields (`/v1/chat/completions`)

Required:

- `model`
- `messages` (array of `{role, content}`)

Supported (optional):

- `temperature` (0..2, finite)
- `max_tokens` / `max_completion_tokens` (only one, or both set to the same value)

Not supported (rejected if present):

- `stream=true` (only non-streaming is supported)
- `n!=1`
- `top_p`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`
- `tools`, `tool_choice`, `response_format`
- `functions`, `function_call`
- `logprobs`, `top_logprobs`

Ignored (commonly sent by clients):

- `user`

### Prompt/result constraints

- Prompt is built from `messages` using a simple text template and is limited by `OPENAI_SERVE_MAX_PROMPT_BYTES`.
- Request body is limited to 1 MiB.
- Provider results must be UTF-8 (non-UTF-8 results return a 502).
- Token usage is approximate (bytes/4 heuristic).

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

`POST /v1/chat/completions` includes LCP metadata headers:

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
