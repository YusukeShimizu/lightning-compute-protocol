# Configuration (high-level)

This document summarizes what to configure and where.

## Minimal configuration

- Requester-only: set `LCPD_LND_*` (peer messaging / invoice payments) and run `lcpd-grpcd`.
- Provider: also set `LCPD_PROVIDER_CONFIG_PATH` (YAML) and `LCPD_BACKEND` (for example `openai`).

## Environment variables (required/optional)

### go-lcpd (common)

| Variable                                  |                            Required | Purpose                                                    |
| ----------------------------------------- | ----------------------------------: | ---------------------------------------------------------- |
| `LCPD_LOG_LEVEL`                          |                            Optional | `debug`/`info`/`warn`, etc.                                |
| `LCPD_BACKEND`                            |   Effectively required for Provider | `openai` / `deterministic` / `disabled`                    |
| `LCPD_OPENAI_API_KEY` or `OPENAI_API_KEY` | Required when `LCPD_BACKEND=openai` | OpenAI API key                                             |
| `LCPD_OPENAI_BASE_URL`                    |                            Optional | Base URL for an OpenAI-compatible API                      |
| `LCPD_DETERMINISTIC_OUTPUT_BASE64`        |                            Optional | Fixed output for the `deterministic` backend (dev/testing) |

### lnd integration (peer messaging / payments)

| Variable                            |               Required | Purpose                                                                 |
| ----------------------------------- | ---------------------: | ----------------------------------------------------------------------- |
| `LCPD_LND_RPC_ADDR`                 |  Required if using lnd | `host:port`                                                             |
| `LCPD_LND_TLS_CERT_PATH`            |       Usually required | Path to `tls.cert` (if empty, verify with system roots)                 |
| `LCPD_LND_ADMIN_MACAROON_PATH`      | Recommended on mainnet | Admin macaroon (needed for payments/invoice ops)                        |
| `LCPD_LND_MANIFEST_RESEND_INTERVAL` |               Optional | Periodically re-send `lcp_manifest` to connected peers (unset/0s disables) |

Typical default paths (adjust for your network and lnd setup):

```sh
export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"
```

### Logging (privacy)

`lcpd-grpcd` logs are intentionally designed to be diagnosable **without** persisting raw user content.

- `LCPD_LOG_LEVEL` controls verbosity (`debug`, `info`, `warn`, `error`; default is `info`).
- Logs MUST NOT contain raw prompts, raw model outputs, API keys, macaroons, or BOLT11 invoices.
- Even with content redaction, logs still contain metadata (peer ids, call ids, prices, timings).

Details: [Logging & privacy](/go-lcpd/docs/logging).

### Provider (YAML)

| Variable                    |              Required | Purpose                                                                                        |
| --------------------------- | --------------------: | ---------------------------------------------------------------------------------------------- |
| `LCPD_PROVIDER_CONFIG_PATH` | Required for Provider | Path to Provider YAML config (default: uses `config.yaml` in the current directory if present) |

Provider configuration is YAML-first (`LCPD_PROVIDER_CONFIG_PATH`). Without YAML, Provider mode is disabled.

Provider YAML details and examples are documented below in "Provider configuration (YAML)".

## Provider configuration (YAML)

Provider behavior for `lcpd-grpcd` is configured via YAML only.

- Pass the path via `LCPD_PROVIDER_CONFIG_PATH`
- If unset, `config.yaml` in the current directory is used (if present)
- If the file is empty or missing, defaults apply (Provider disabled, TTL=300s, `max_output_tokens=4096`, built-in price table, etc.)

Sample with explicit defaults: `go-lcpd/config.yaml.sample` (copy to `go-lcpd/config.yaml`)

### Examples

#### mainnet example

```yaml
enabled: true
quote_ttl_seconds: 600

pricing:
  # Optional: load-based surge pricing, applied at quote-time only.
  # Multiplier = 1.0 + per_job_bps/10_000 * max(0, in_flight_jobs - threshold)
  in_flight_surge:
    threshold: 2
    per_job_bps: 500 # +5% per job above threshold
    max_multiplier_bps: 30000 # 3.0x cap

llm:
  max_output_tokens: 4096
  models:
    gpt-5.2:
      # Optional: per-model override for max output tokens.
      # max_output_tokens: 4096

      # Required: pricing (msat per 1M tokens).
      price:
        input_msat_per_mtok: 1750000
        cached_input_msat_per_mtok: 175000
        output_msat_per_mtok: 14000000
```

#### regtest example

```yaml
enabled: true
quote_ttl_seconds: 60

llm:
  max_output_tokens: 512
  models:
    gpt-5.2:
      max_output_tokens: 512
      price:
        input_msat_per_mtok: 1
        output_msat_per_mtok: 1
```

### Model naming

- `model` is the OpenAI model ID.
- It appears in:
  - `openai_chat_completions_v1_params_tlvs.model` on the wire (in `lcp_call.params`)
  - the OpenAI request JSON (`request_json.model`) carried in the request stream bytes
- The Provider uses `model` for allowlisting, pricing, and routing to the compute backend.

### Field reference

- `enabled`: Enables Provider mode. If `false`, rejects quote/cancel and does not create invoices.
- `quote_ttl_seconds`: Quote and invoice TTL in seconds. Default is 300s.
- `pricing.in_flight_surge`: Optional load-based surge pricing, computed from the current in-flight job count at quote time.
  - `threshold`: Number of in-flight jobs before surge applies.
  - `per_job_bps`: Additive multiplier per job above `threshold` (basis points; 10,000 = 1.0x). If `0`, surge is disabled.
  - `max_multiplier_bps`: Caps the total multiplier in basis points. If `0`, a safe default cap is used.
- `llm.max_output_tokens`: Provider-wide default for output token limits, used for quote-time estimation and request validation. Default is 4096.
- `llm.models`: Map of allowed `openai.*` model IDs. If empty, accepts any `model` (providers may still apply their own policy).
  - `max_output_tokens`: Optional per-model override (must be > 0).
  - `price`: Required per-model pricing (msat per 1M tokens). `input_msat_per_mtok` and `output_msat_per_mtok` are required; `cached_input_msat_per_mtok` is optional.

### Default price table

If YAML is not provided, a built-in price table is used (msat per 1M tokens):
- `gpt-5.2`: input 1,750,000 / cached 175,000 / output 14,000,000

### Quote → Execute flow (`openai.chat_completions.v1`)

1. Validate the QuoteRequest and check the model is allowed.
2. Receive and validate the request stream as OpenAI request JSON bytes (`request_json.model`, `request_json.messages`, optional `request_json.stream`).
3. Determine `max_output_tokens` for quote-time estimation:
   - Start from `llm.max_output_tokens` (and optional per-model override).
   - If the request sets an output-token cap (`max_completion_tokens` / `max_tokens` / `max_output_tokens`), it must be `<=` the Provider max, and that value is used for estimation.
4. Estimate token usage via `UsageEstimator` (`approx.v1`: `ceil(len(bytes)/4)`).
5. Compute price in msat via `QuotePrice(model, estimate, cached=0, price_table)` and apply optional `pricing.in_flight_surge`, then embed it into TermsHash / invoice binding.
6. After payment settles, execute the passthrough request in the backend, stream the response (`lcp_stream_*`), and finalize with `lcp_complete`.

## Backend notes

- `openai`: calls an external API (billing / rate limits / network dependency).
  - Uses the OpenAI-compatible Chat Completions API (`POST /v1/chat/completions`).
  - Sends the raw request body bytes from the LCP input stream as-is (non-streaming).
  - Returns the raw OpenAI-compatible response body bytes as-is (non-streaming JSON).
- `deterministic`: fixed-output backend for development (no external API).
- `disabled`: does not execute (useful for Requester-only mode).

## Related docs

- gRPC dev CLI: `cli.md`
- mainnet walkthrough: `quickstart-mainnet.md`
- regtest walkthrough: `regtest.md`
