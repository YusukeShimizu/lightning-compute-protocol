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
| `LCPD_LND_MANIFEST_RESEND_INTERVAL` |               Optional | Interval for re-sending `lcp_manifest` (e.g., `30s`, disable with `0s`) |

Typical default paths (adjust for your network and lnd setup):

```sh
export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"
```

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

Sample with explicit defaults: `../config.yaml`

### Examples

#### mainnet example

```yaml
enabled: true
quote_ttl_seconds: 600

llm:
  max_output_tokens: 4096
  chat_profiles:
    gpt-5.2:
      # Optional: if omitted, backend_model defaults to the profile name.
      # backend_model: gpt-5.2

      # Optional: per-profile override for max output tokens.
      # max_output_tokens: 4096

      # Required: pricing (msat per 1M tokens).
      price:
        input_msat_per_mtok: 1750000
        cached_input_msat_per_mtok: 175000
        output_msat_per_mtok: 14000000

      # Optional: OpenAI-compatible Chat Completions parameters (Provider-side defaults).
      # openai:
      #   temperature: 0.7
      #   top_p: 1
      #   stop: ["\\n\\n"]
```

#### regtest example

```yaml
enabled: true
quote_ttl_seconds: 60

llm:
  max_output_tokens: 512
  chat_profiles:
    gpt-5.2:
      max_output_tokens: 512
      price:
        input_msat_per_mtok: 1
        output_msat_per_mtok: 1
```

### Model / profile naming

- `profile` is the LCP wire identifier (`llm_chat_params.profile`). It appears in:
  - `lcp_manifest.supported_tasks[].llm_chat.profile` (advertising)
  - the Provider-side pricing lookup (invoice/terms binding)
- `backend_model` is the upstream model ID passed to the compute backend (for example OpenAI-compatible `model`).
- `profile` and `backend_model` do not have to match. Mapping is configured per-profile via `llm.chat_profiles.*.backend_model`.

### Field reference

- `enabled`: Enables Provider mode. If `false`, rejects quote/cancel and does not create invoices.
- `quote_ttl_seconds`: Quote and invoice TTL in seconds. Default is 300s.
- `llm.max_output_tokens`: Provider-wide default for execution policy (`ExecutionPolicy`). Applied both to quote-time estimation and backend execution. Default is 4096.
- `llm.chat_profiles`: Map of allowed/advertised `llm.chat` profiles. If empty, accepts any profile but does not advertise them in the manifest.
  - `backend_model`: Upstream model ID passed to the backend. Defaults to the profile name.
  - `max_output_tokens`: Optional per-profile override (must be > 0).
  - `price`: Required per-profile pricing (msat per 1M tokens). `input_msat_per_mtok` and `output_msat_per_mtok` are required; `cached_input_msat_per_mtok` is optional.
  - `openai`: Optional OpenAI-compatible Chat Completions parameters (`temperature`, `top_p`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`).

### Default price table

If YAML is not provided, a built-in price table is used (msat per 1M tokens):
- `gpt-5.2`: input 1,750,000 / cached 175,000 / output 14,000,000

### Quote → Execute flow (`llm.chat`)

1. Validate the QuoteRequest and check the profile is allowed.
2. Apply ExecutionPolicy (`max_output_tokens`) to the `computebackend.Task`.
3. Estimate token usage via `UsageEstimator` (`approx.v1`: `ceil(len(bytes)/4)`).
4. Compute price in msat via `QuotePrice(profile, estimate, cached=0, price_table)` and embed it into TermsHash / invoice binding.
5. After payment settles, map `profile -> backend_model`, execute the planned task in the backend, and return the result via `lcp_result`.

## Backend notes

- `openai`: calls an external API (billing / rate limits / network dependency).
  - Uses the OpenAI-compatible Chat Completions API (`POST /v1/chat/completions`).
  - `llm.chat` `profile` is mapped to the upstream `model` via `llm.chat_profiles.*.backend_model` (defaults to the profile name).
  - Supports `params_bytes` JSON:
    - `max_output_tokens` (and legacy `max_tokens`), sent as `max_completion_tokens`
    - `temperature`, `top_p`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`
  - Sends a single text user message (no tools, no multimodal inputs, no streaming).
- `deterministic`: fixed-output backend for development (no external API).
- `disabled`: does not execute (useful for Requester-only mode).

## Related docs

- gRPC dev CLI: `cli.md`
- mainnet walkthrough: `quickstart-mainnet.md`
- regtest walkthrough: `regtest.md`
