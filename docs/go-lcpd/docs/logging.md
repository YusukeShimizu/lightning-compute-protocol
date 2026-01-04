# Logging & privacy (lcpd-grpcd / go-lcpd)

This project treats logs as **sensitive**. Logs are meant to let you reconstruct what happened (quote → pay → result)
without ever persisting raw user content.

## Hard rules (MUST NOT)

- MUST NOT log raw request JSON (`openai_chat_completions_v1.request_json` / wire `input` stream bytes).
- MUST NOT log raw model outputs (raw result stream bytes / gRPC `Result.result`).
- MUST NOT log secrets: API keys, macaroons, access tokens.
- MUST NOT log BOLT11 `payment_request` strings (invoices).
- MUST NOT log raw Lightning custom-message payloads or full gRPC request/response objects.

## What is safe to log (examples)

The code favors logging **metadata** only:

- Correlation: `job_id`, `peer_id` / `peer_pub_key`
- Task metadata: `task_kind`, `model`, `input_bytes`
- Quote/payment: `price_msat`, `quote_expiry_unix`
- Timing: `quote_ms`, `pay_ms`, `wait_ms`, `execute_ms`, `total_ms`
- Output metadata: `output_bytes`, `content_type`, `usage_*` (token units when available)

## Log levels

`LCPD_LOG_LEVEL` controls verbosity (`debug`, `info`, `warn`, `error`).

- `error`: service-level failures (unexpected / cannot proceed).
- `warn`: per-job failures or anomalous events (still no prompt/output).
- `info`: lifecycle summaries that allow measurement and correlation (quote issued/received, result received, job completed).
- `debug`: additional details for diagnosis (drops, resends, replay handling), still no secrets.

## Operational notes

- Even with content redaction, logs still contain **metadata** (peer ids, job ids, prices, timings). Treat log storage and
  retention as a security decision.
- If you write logs to disk, use restrictive permissions and rotation. See
  [Background run + logging](/go-lcpd/docs/background).
