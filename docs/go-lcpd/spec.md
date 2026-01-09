# go-lcpd spec (WYSIWID)

This document is the implementation specification (single source of truth) for `go-lcpd/` in this repository.
It is written in WYSIWID format (Concepts + Synchronizations).

Sources of truth:

- gRPC API: `go-lcpd/proto/lcpd/v1/lcpd.proto`
- LCP wire protocol: `protocol/protocol.md` (LCP v0.3, `protocol_version=3`)

`go-lcpd` exchanges LCP messages (TLV streams) with peers over `lnd` BOLT #1 custom messages as the transport.
It exposes a gRPC API (`lcpd.v1.LCPDService`) for local clients to drive the requester-side LCP flow:

**call → quote → pay → stream(response) → complete**

The same process can also run as a provider-side handler (when configured) to process inbound `lcp_call`, receive the
request stream, return a quote + invoice (`lcp_quote`), and then stream the response and send `lcp_complete`.
It does not implement its own P2P (TCP) transport.

Protobuf is managed via `buf`. The goal is to keep the following integration tests reproducible within this project:

- gRPC: smoke check `GetLocalInfo` / `ListLCPPeers` (no external processes)
- Lightning (custom messages, regtest): two nodes exchange `lcp_manifest` and `ListLCPPeers` can enumerate LCP-capable peers (opt-in)
- Lightning (LCP provider, regtest): complete inbound `lcp_call` → request stream → invoice → response stream → `lcp_complete` over custom messages (opt-in)
- Lightning (LCP requester, regtest): receive response stream + `lcp_complete` via `RequestQuote` → `AcceptAndExecute` (opt-in)

## Security & Architectural Constraints

These constraints are system invariants.

- Do not modify BOLTs: `go-lcpd` MUST NOT propose or require changes to Lightning BOLT specs (L2). Rationale: preserve interoperability.
- Do not implement a custom P2P (TCP) transport: `go-lcpd` MUST NOT implement peer transport over TCP/UDP. Delegate peer messaging to `lnd` peer connections. Rationale: avoid re-implementation and reduce operational risk.
- LCP message types MUST match `protocol/protocol.md`: type numbers for `lcp_manifest`, etc. MUST follow the odd assignments in `protocol/protocol.md`. Rationale: coexistence with non-LCP peers.
- BOLT #1 unknown message rule: ignore unknown odd messages. Disconnect on unknown even messages via `lnrpc.Lightning.DisconnectPeer`. Rationale: match BOLT #1 parity behavior.

- Enforce LCP v0.3 wire rules: `protocol_version=3` MUST be used for all LCP messages. Implementations MUST send and accept only version 3. Rationale: interoperability.
- Manifest-gated call messages: `go-lcpd` MUST NOT send any call-scope messages until it has (1) sent `lcp_manifest` and (2) received the peer's `lcp_manifest`. Rationale: required by `protocol/protocol.md` and needed to learn peer limits (`max_payload_bytes`, etc.).

- Preserve TLV stream canonical form: encode TLVs as `bigsize(type)` → `bigsize(length)` → `value`. Sort by type. Do not duplicate types. Rationale: byte-exact `terms_hash` and forward compatibility.
- Do not disconnect on unknown TLVs: unknown TLVs MUST NOT cause disconnection. Continue decoding. Rationale: forward compatibility.

- Always include call-scope envelope TLVs: all LCP messages except `lcp_manifest` MUST include `call_id`, `msg_id`, and `expiry` per `protocol/protocol.md`. Rationale: replay safety and idempotency.
- Deterministic chunk `msg_id`: for `lcp_stream_chunk`, `msg_id` MUST be `SHA256(stream_id || u32be(seq))` per `protocol/protocol.md`. Rationale: bounded replay state for large streams.
- Ignore expired messages: receivers MUST ignore call-scope messages where `expiry < now`. Rationale: replay resistance.
- Clamp envelope expiry window: receivers MUST use `effective_expiry = min(expiry, now + MAX_ENVELOPE_EXPIRY_WINDOW_SECONDS)` per `protocol/protocol.md`. Rationale: prevent state-holding DoS via far-future `expiry`.
- Ignore duplicate messages: receivers MUST de-duplicate by (`call_id`, `msg_id`) until `effective_expiry`. Rationale: idempotency.
  - For `lcp_stream_chunk`, implementations MUST treat `seq < expected_seq` as a duplicate and MUST ignore it (without storing per-chunk replay entries).

- LCP v0.3 `terms_hash`: compute `terms_hash` byte-exactly as defined in `protocol/protocol.md` (including canonicalization of method `params` and request/response stream metadata). Rationale: invoice binding depends on byte-exactness.
- Provider execution rule: Providers MUST NOT execute or deliver responses for `call_id` before invoice settlement. Rationale: `protocol/protocol.md` execution rule.
- Requester invoice checks: before paying, verify at least:
  - `description_hash == terms_hash`
  - invoice payee pubkey matches the Provider peer pubkey
  - invoice amount matches `price_msat` (amount-less invoices are rejected)
  - invoice expiry does not exceed `quote_expiry` (allow small clock skew)

- `openai.*` params TLVs are strict: Providers MUST reject unknown param types (and invalid TLV encoding). Rationale: fixed interpretation of typed params.

## Concepts

### LCPWireCodec

Purpose: Encode/decode LCP v0.3 custom message payloads (TLV streams) used on the wire.

Domain Model (concept-local):

- `Manifest`: protocol_version, max_payload_bytes, max_stream_bytes, max_call_bytes, max_inflight_calls?, supported_methods[]
- `CallEnvelope`: protocol_version, call_id(32 bytes), msg_id(32 bytes), expiry(tu64)
- `Call`: envelope, method(string), params_bytes?(bytes), params_content_type?(string)
- `Quote`: envelope, price_msat, quote_expiry, terms_hash(32 bytes), payment_request(string), response_content_type?, response_content_encoding?
- `Complete`: envelope, status(ok|failed|cancelled), message?, response_stream_id?, response_hash?, response_len?, response_content_type?, response_content_encoding?
- `StreamBegin`: envelope, stream_id(32 bytes), stream_kind(request|response), content_type, content_encoding, total_len?, sha256?
- `StreamChunk`: envelope, stream_id, seq(u32), data(bytes)
- `StreamEnd`: envelope, stream_id, total_len(tu64), sha256(32 bytes)
- `Cancel`: envelope, reason?
- `Error`: envelope, code(u16), message?

Actions:

- `encode_* (message) -> payload_bytes | error`
- `decode_* (payload_bytes) -> message | error`

Operational Principle:

- All payloads are TLV streams and MUST follow canonical TLV rules.
- Encoding/decoding MUST be round-trip safe for all known fields, and MUST ignore unknown TLVs.

### LCPMessageRouter

Purpose: Classify inbound custom messages into known LCP message types and apply the BOLT #1 parity rule for unknown message types.

Domain Model (concept-local):

- `CustomMessage`: peer_pubkey, msg_type(uint16), payload_bytes
- `RouteDecision`: action(dispatch_manifest|dispatch_call|dispatch_quote|dispatch_stream_begin|dispatch_stream_chunk|dispatch_stream_end|dispatch_complete|dispatch_cancel|dispatch_error|ignore|disconnect), reason

Actions:

- `route(custom_message) -> decision`

Operational Principle:

- LCP v0.3 message types follow the assignments in `protocol/protocol.md` (all odd).
- Unknown odd is ignored; unknown even triggers disconnect (BOLT #1 parity rule).
- `route` does not decode the payload (classification only).

### ReplayStore

Purpose: Provide idempotency (de-duplication) and replay safety for call-scope messages.

Domain Model (concept-local):

- `ReplayKey`: peer_pubkey, call_id(32 bytes), msg_id(32 bytes)
- `ReplayEntry`: key, expiry(uint64)

Actions:

- `check_and_remember(key, effective_expiry, now) -> ok | duplicate | expired`
- `prune(now) -> void`

Operational Principle:

- Messages with `expiry < now` MUST be treated as `expired` and ignored.
- `duplicate` entries MUST be retained until `effective_expiry`.
- The store MUST be pruned by TTL and MUST have bounds (e.g., max entries) to cap memory usage.

### RequesterCallStore

Purpose: Hold requester-side call state (call spec, quote, payment state, completion) to support retries, cancellation, and gRPC workflows.

Domain Model (concept-local):

- `CallKey`: peer_id, call_id
- `CallState`: quoted | paid | completed | cancelled | failed
- `CallSpec`: method, params_bytes, request_bytes, request_content_type, request_content_encoding
- `QuoteRecord`: quote, received_at
- `CompletionRecord`: complete, received_at
- `CallRecord`: key, spec, quote_record?, completion_record?, state, created_at, updated_at

Actions:

- `put_spec(key, spec) -> void`
- `put_quote(key, quote) -> void`
- `put_complete(key, complete) -> void`
- `get(key) -> record | not_found`
- `mark_state(key, state) -> void`

Operational Principle:

- Calls MUST be keyed by `peer_id` as well as `call_id` to avoid cross-peer collisions.
- Records MUST be bounded and SHOULD be GC'd after completion (and/or TTL).

### ProviderCallStore

Purpose: Hold provider-side call state (request stream, quote, payment tracking, execution state, response streaming) to support idempotency and cancellation.

Domain Model (concept-local):

- `CallKey`: peer_id, call_id
- `CallState`: awaiting_request | quoted | waiting_payment | paid | executing | streaming_response | done | cancelled | failed
- `RequestStreamState`: stream_id, content_type, content_encoding, expected_seq, buf, total_len, sha256, validated
- `QuoteRecord`: quote, payment_request, quote_expiry, terms_hash, created_at
- `CallRecord`: key, method, params_bytes, request_stream_state?, quote_record?, state, created_at, updated_at

Actions:

- `get(key) -> record | not_found`
- `upsert(record) -> void`
- `update_state(key, state) -> void`
- `set_quote(key, quote_record) -> void`
- `set_request_stream(key, request_stream_state) -> void`

Operational Principle:

- Store MUST have bounds and MUST NOT retain pending state past quote expiry.
- For calls awaiting request stream completion, implementations SHOULD evict state once the envelope expiry retention window passes.

### StreamAssembler

Purpose: Assemble `lcp_stream_*` messages into decoded bytes with validation (seq ordering + sha256/len at end).

Domain Model (concept-local):

- `StreamKey`: peer_id, call_id, stream_id
- `ExpectedSeq`: next expected chunk sequence number
- `StreamBuffer`: bytes, expected_seq
- `StreamMeta`: content_type, content_encoding, total_len, sha256

Actions:

- `accept_begin(begin) -> ok | error`
- `accept_chunk(chunk) -> ok | duplicate | error`
- `accept_end(end) -> decoded_bytes | error`

Operational Principle:

- `seq` MUST start at 0 and increase by exactly 1.
- For `seq < expected_seq`, treat as duplicate and ignore.
- At end, MUST validate `total_len` and `sha256` against decoded bytes.

### ComputeBackend

Purpose: Execute supported `openai.*` methods on the Provider side and produce response bytes/streams.

Domain Model (concept-local):

- `Method`: string (e.g. `openai.chat_completions.v1`, `openai.responses.v1`)
- `Model`: string
- `RequestBytes`: bytes (raw OpenAI-compatible request body)
- `ResponseBody`: io stream (raw OpenAI-compatible response body; may be `text/event-stream` for streaming requests)

Actions:

- `execute(ctx, method, model, request_bytes) -> response_body_stream | error`

Operational Principle:

- `model` is taken from method params TLVs (not from request JSON).
- `request_bytes` is passed through byte-for-byte to the upstream OpenAI-compatible endpoint.
- Implementations MUST bound maximum response bytes and MUST fail safely on oversized responses.

## Synchronizations

### sync ManifestExchange

Summary: Exchange `lcp_manifest` with a peer and track the peer's limits/capabilities.

Flow:

1. When: `go-lcpd` observes a peer connection event (lnd peer).
2. Then: send local `lcp_manifest` (v0.3).
3. When: `lcp_manifest` is received from the peer.
4. Then: decode it, validate `protocol_version=3`, and record it for the connection.
5. Then: mark the peer as LCP-ready only after both local and remote manifests are known.

Error branches:

- If the peer's manifest has unsupported `protocol_version`, mark the peer as not ready.
- If a manifest fails TLV decoding, ignore it and keep the peer not ready.

### sync RequestQuote (Requester)

Summary: Send `lcp_call` + request stream to a Provider and receive `lcp_quote`.

Flow:

1. When: gRPC `RequestQuote(peer_id, call_spec)` is called.
2. Then: ensure the peer is connected and LCP-ready (manifest exchange completed).
3. Then: generate a fresh `call_id` and store `call_spec` in `RequesterCallStore`.
4. Then: send `lcp_call` with (`call_id`, `method`, optional `params`).
5. Then: send the request stream (`stream_kind=request`) using `lcp_stream_begin/chunk/end`:
   - content_type/content_encoding come from `call_spec`.
   - enforce remote limits (`max_payload_bytes`, `max_stream_bytes`, `max_call_bytes`).
6. Then: wait for either:
   - `lcp_quote` for that `call_id` (success), OR
   - `lcp_error` for that `call_id` (failure), OR
   - deadline exceeded (unknown outcome).
7. Then: on `lcp_quote`, store quote in `RequesterCallStore` and return it to the client.

### sync AcceptAndExecute (Requester)

Summary: Pay the invoice from a previously received quote and wait for `lcp_complete` and the validated response stream.

Flow:

1. When: gRPC `AcceptAndExecute(peer_id, call_id, pay_invoice=true)` is called.
2. Then: load the stored quote for (`peer_id`, `call_id`) from `RequesterCallStore` (NOT_FOUND if absent).
3. Then: verify quote expiry and validate the invoice binding preconditions.
4. Then: pay the BOLT11 invoice via `lnd` (block until settled or deadline).
5. Then: wait for the response stream (`stream_kind=response`) and assemble it via `StreamAssembler`.
6. Then: wait for terminal `lcp_complete` and validate it against the stream metadata (hash/len/content-type/encoding).
7. Then: store completion in `RequesterCallStore` and return `Complete` to the client.

Streaming variant:

- `AcceptAndExecuteStream` emits:
  - `response_begin` (content-type/encoding),
  - `response_chunk` (decoded bytes),
  - `response_end` (hash/len),
  - `complete` (terminal status).

### sync ProviderHandleCall (Provider)

Summary: As a Provider, receive `lcp_call`, receive the request stream, issue an invoice-bound quote, wait for settlement, stream the response, and send `lcp_complete`.

Flow:

1. When: Provider handler receives `lcp_call` for `call_id`.
2. Then: validate:
   - `protocol_version=3`,
   - `method` is supported,
   - `params` are present and valid for the method (strict TLV).
3. Then: receive and validate exactly one request stream (`stream_kind=request`) for this `call_id`:
   - enforce chunk ordering,
   - validate sha256/len at end.
4. Then: estimate usage / price and build `terms_hash` per wire spec.
5. Then: create a BOLT11 invoice with `description_hash=terms_hash` and amount=`price_msat`.
6. Then: send `lcp_quote` containing `price_msat`, `terms_hash`, `quote_expiry`, and `payment_request`.
7. Then: wait for invoice settlement (best-effort with TTL).
8. Then: execute the method via `ComputeBackend`, streaming the response:
   - send `lcp_stream_begin/chunk/end` with `stream_kind=response`.
9. Then: send terminal `lcp_complete(status=ok)` including response metadata (hash/len/content-type/encoding).

Error branches:

- If validation fails before quoting, send `lcp_error(code=unsupported_method|invalid_state|...)`.
- If cancelled (`lcp_cancel`) before settlement, stop and send `lcp_complete(status=cancelled)`.
- If execution fails after settlement, send `lcp_complete(status=failed, message=...)`.

### sync CancelJob (Requester)

Summary: Send `lcp_cancel` to the provider for an in-flight call.

Flow:

1. When: gRPC `CancelJob(peer_id, call_id)` is called.
2. Then: ensure the peer is connected.
3. Then: send `lcp_cancel` for that `call_id` (best-effort).
4. Then: mark the call as cancelled locally (best-effort; the provider may still complete).

