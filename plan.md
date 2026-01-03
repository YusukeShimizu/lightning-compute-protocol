# Add `openai.chat_completions.v1` passthrough over LCP v0.2

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository defines ExecPlan requirements in `.agent/PLANS.md`. Maintain this document in accordance with that file.

## Purpose / Big Picture

Enable OpenAI-compatible clients to call `POST /v1/chat/completions` against `apps/openai-serve/` and have the exact HTTP request body JSON transported over LCP v0.2 to a Provider, executed, and returned as the exact OpenAI-compatible response body bytes.

After this change, `openai-serve` becomes a thin HTTP gateway instead of a strict schema decoder and prompt templater: it no longer rejects unknown request fields and it does not re-encode `tools`, `tool_choice`, `response_format`, etc. Those fields are treated as opaque JSON and are transported end-to-end.

MVP scope is non-streaming only: `stream:true` is rejected at the HTTP edge with a 400, and the Provider also rejects it defensively if it is received through other clients.

## Progress

- [x] (2026-01-02 05:52Z) Converted the draft `plan.md` into ExecPlan format aligned with `.agent/PLANS.md`.
- [x] (2026-01-02 21:40Z) Surveyed the current `llm.chat` implementation end-to-end and identified edit points for adding a new task kind.
- [x] (2026-01-02 21:40Z) Extended the wire protocol docs with `task_kind="openai.chat_completions.v1"` (English + Japanese).
- [x] (2026-01-02 21:40Z) Extended `go-lcpd/proto/lcpd/v1/lcpd.proto` and regenerated protobuf outputs in both `go-lcpd/` and `apps/openai-serve/`.
- [x] (2026-01-02 22:36Z) Implemented Requester support: task validation + quote/input construction for `openai.chat_completions.v1`.
- [x] (2026-01-02 22:36Z) Implemented Provider support: validation, execution flow, and result metadata for passthrough JSON.
- [x] (2026-01-02 23:47Z) Updated `apps/openai-serve` to passthrough request/response bytes, allow unknown fields, and updated tests/docs.
- [x] (2026-01-02 23:47Z) Ran module tests + golangci-lint and confirmed acceptance criteria (completed: `go-lcpd`, `apps/openai-serve`).

## Surprises & Discoveries

- Observation: `docs/protocol/protocol-ja.md` is a Japanese v0.1 overview (not a full v0.2 translation).
  Evidence: The doc header says “LCP v0.1 仕様（日本語概要）”; the normative v0.2 spec lives in `docs/protocol/protocol.md`.

## Decision Log

- Decision: The MVP rejects `stream:true` at the HTTP boundary with status 400 and also rejects it in Provider-side validation for defense-in-depth.
  Rationale: Keeps semantics simple while we define a future-compatible streaming encoding (SSE passthrough) without committing to chunk boundary validation now.
  Date/Author: 2026-01-02 (Codex)

- Decision: `openai-serve` and `go-lcpd` treat unknown JSON fields as opaque and never reject based on schema evolution.
  Rationale: OpenAI request/response formats evolve frequently; passthrough keeps compatibility and avoids constant gateway updates.
  Date/Author: 2026-01-02 (Codex)

- Decision: For `task_kind="openai.chat_completions.v1"`, model discovery is represented as a TLV param `model` and Providers advertise supported models by emitting one `supported_tasks` template per model.
  Rationale: LCP v0.2 `supported_tasks` is a list of `(task_kind, params_template)` pairs; repeating templates (one per model) fits the existing matching model without introducing a new “list-of-models” schema.
  Date/Author: 2026-01-02 (Codex)

- Decision: For `task_kind="openai.chat_completions.v1"`, `params.model` MUST match `request_json.model` (validated in both Requester and Provider).
  Rationale: The Provider executes the raw request JSON; requiring a match prevents quoting/routing based on one model while executing another.
  Date/Author: 2026-01-02 (Codex)

## Outcomes & Retrospective

- Outcome: `apps/openai-serve` now forwards the raw `POST /v1/chat/completions` request JSON bytes over LCP v0.2 using `task_kind="openai.chat_completions.v1"` and returns the raw OpenAI-compatible response body bytes as-is.
- Outcome: Unknown request fields (including `tools`, `tool_choice`, `response_format`, etc.) are accepted and preserved end-to-end; `stream:true` is rejected with HTTP 400.
- Validation: `cd apps/openai-serve && go test ./...` and `make lint` pass.

## Context and Orientation

LCP (Lightning Compute Protocol) v0.2 is an application-layer protocol over Lightning BOLT #1 custom messages. A Requester and Provider exchange `lcp_manifest`, then the Requester sends `lcp_quote_request`, streams input (`lcp_stream_*` with `stream_kind=input`), receives `lcp_quote_response` (including an invoice bound to the job terms), pays, then receives a result stream (`stream_kind=result`) and a terminal `lcp_result`.

In this repository:

- `docs/protocol/protocol.md` defines the LCP v0.2 wire protocol, including `task_kind="llm.chat"` (§5.2.1), `task_kind="openai.chat_completions.v1"` (§5.2.1), and the `supported_tasks` manifest field used for model discovery.
- `docs/protocol/protocol-ja.md` is a Japanese overview; the English doc is the SSOT and normative specification.
- `go-lcpd/` is the reference implementation. The gRPC daemon `go-lcpd/tools/lcpd-grpcd` is the local Requester component used by `apps/openai-serve`.
- `apps/openai-serve/` is an OpenAI-compatible HTTP gateway that forwards requests to `lcpd-grpcd` over gRPC.

Current behavior (before this work):

- `apps/openai-serve/internal/httpapi/handler_chat.go` strictly decodes chat requests using `DisallowUnknownFields` (`apps/openai-serve/internal/httpapi/request_decode.go`), rejects many OpenAI fields (including `tools` and `response_format`), renders `messages` into a prompt string, and sends an `llm.chat` gRPC task (`lcpdv1.Task{llm_chat: ...}`).
- The Provider executes `llm.chat` using `go-lcpd/internal/computebackend/openai` which calls an OpenAI-compatible `/v1/chat/completions` endpoint via the OpenAI Go SDK and returns only the assistant content as a UTF-8 string (not the full JSON response).

This ExecPlan adds a new strict, typed gRPC task kind that transports the OpenAI request body JSON as bytes and returns the OpenAI response body bytes as-is.

### Survey: current `llm.chat` edit points

The existing `llm.chat` path is spread across `apps/openai-serve/` (HTTP gateway), `go-lcpd/` (Requester gRPC + wire encoding), and Provider execution code. These are the concrete edit points that must be extended or refactored to introduce a new task kind:

In `apps/openai-serve/` (HTTP → gRPC task construction + routing):

- `apps/openai-serve/internal/httpapi/handler_chat.go`: `handleChatCompletions`, `buildLCPChatTask`, `validateChatRequest`, and the response construction that assumes UTF-8 assistant text.
- `apps/openai-serve/internal/httpapi/request_decode.go`: `decodeJSONBody` uses `dec.DisallowUnknownFields()` (strict decoding).
- `apps/openai-serve/internal/httpapi/routing.go`: model discovery/routing via `supported_tasks` currently matches only `LCP_TASK_KIND_LLM_CHAT` with `tmpl.llm_chat.profile`.

In `go-lcpd/` (gRPC service → wire quote + streams):

- `go-lcpd/internal/grpcservice/lcpd/service.go`: `RequestQuote`, `buildQuoteRequestPayload` (calls `lcptasks.ToWireQuoteRequestTask` and `lcptasks.ToWireInputStream`), and `toProtoManifest` (translates wire `supported_tasks` into proto `LCPTaskTemplate`).
- `go-lcpd/internal/lcptasks/lcptasks.go`: `ValidateTask`, `ToWireQuoteRequestTask`, and `ToWireInputStream` (currently only `llm.chat`).
- `go-lcpd/internal/lcpwire/quote_request.go` and `go-lcpd/internal/lcpwire/manifest.go`: task_kind-dependent `params` handling and TLV encoding/decoding.

In Provider mode (validation, quoting, execution, capability advertisement):

- `go-lcpd/internal/provider/validation.go`: `DefaultValidator`, `ValidateQuoteRequest`, and `matchesTemplate` (task-kind-specific param validation/matching).
- `go-lcpd/internal/provider/handler.go`: quote/execution planning and pricing (`planTask`, `quotePrice`, and `rejectQuoteRequestIfUnsupportedProfile`) assumes `llm.chat` semantics.
- `go-lcpd/tools/lcpd-grpcd/main.go`: `localManifestForProvider` builds the local `supported_tasks` list from configured `llm.chat` profiles.
- `go-lcpd/internal/llm/policy.go` and `go-lcpd/internal/llm/estimator.go`: enforce `task.TaskKind == "llm.chat"`.
- `go-lcpd/internal/computebackend/openai/backend.go`: executes only `task.TaskKind == "llm.chat"` and returns only assistant content, not raw JSON.

## Plan of Work

First, extend the protocol documentation to define a new task kind:

Define `task_kind = "openai.chat_completions.v1"` as “OpenAI Chat Completions v1 passthrough”. For this task kind, the decoded input stream bytes are the exact UTF-8 JSON bytes of the HTTP request body for `POST /v1/chat/completions`. The decoded result stream bytes are the exact bytes of the OpenAI-compatible HTTP response body (non-streaming JSON in the MVP).

Next, extend the gRPC API in `go-lcpd/proto/lcpd/v1/lcpd.proto` so `apps/openai-serve` can submit the passthrough request to `lcpd-grpcd` without reinterpreting fields. Add:

- A new enum value `LCP_TASK_KIND_OPENAI_CHAT_COMPLETIONS_V1` mapped to string `"openai.chat_completions.v1"`.
- A new `Task` oneof case `openai_chat_completions_v1` holding `OpenAIChatCompletionsV1TaskSpec` with `bytes request_json` and required `OpenAIChatCompletionsV1Params params` (including `model`).
- A new `LCPTaskTemplate` oneof case `openai_chat_completions_v1` containing `OpenAIChatCompletionsV1Params` (including `model`) for capability advertisement (one template per supported model ID).

Then, implement Requester-side conversions in `go-lcpd/internal/lcptasks`:

- Validate that `request_json` is valid JSON, top-level object, contains `model` (string) and `messages` (array).
- Reject `stream:true`.
- Map to a wire `lcp_quote_request` with `task_kind="openai.chat_completions.v1"` and (optional) params TLVs.
- Map the input stream metadata to `content_type="application/json; charset=utf-8"` and `content_encoding="identity"` and send the raw request bytes as the decoded bytes.

Then, implement Provider-side support in `go-lcpd/internal/provider`:

- Accept the new task kind in the Provider validator.
- When handling the input stream, perform the minimal JSON validation above and reject `stream:true`.
- Execute by proxying the input JSON to an upstream OpenAI-compatible HTTP endpoint at `POST /v1/chat/completions` and return the raw response body bytes as the result stream bytes with `content_type="application/json; charset=utf-8"` and `content_encoding="identity"`.

Finally, update `apps/openai-serve`:

- Replace strict decoding with raw-body passthrough (still with a 1 MiB body cap) and minimal JSON validation for `model`, `messages`, and `stream`.
- Route by `model` using Provider discovery (`supported_tasks`) and send the raw request bytes via the new gRPC task kind.
- Return the raw LCP result bytes as the HTTP response body (and set `Content-Type` from the LCP result metadata).
- Update docs and tests to reflect that unknown fields and `tools` are accepted.

## Concrete Steps

All commands below assume the repository root as the working directory unless stated otherwise. This repository is multi-module Go; run Go commands from the module directories (`go-lcpd/` and `apps/openai-serve/`) rather than from the repo root.

1) Update protocol docs:

    cd docs
    # edit docs/protocol/protocol.md and docs/protocol/protocol-ja.md

2) Update protobuf definitions and regenerate:

    cd go-lcpd
    make gen  # preferred (Nix-first)

    # If you cannot use Nix, you can run Buf directly (requires network access to fetch buf.build plugins):
    # PATH="$(pwd)/.tools/bin:$PATH" buf generate

    # Sync generated protobufs into openai-serve (this repo keeps a copy there).
    cd ..
    cp go-lcpd/gen/go/lcpd/v1/lcpd.pb.go apps/openai-serve/gen/go/lcpd/v1/lcpd.pb.go
    cp go-lcpd/gen/go/lcpd/v1/lcpd_grpc.pb.go apps/openai-serve/gen/go/lcpd/v1/lcpd_grpc.pb.go

3) Implement Requester and Provider logic in `go-lcpd/` (code edits described in “Plan of Work”).

4) Update openai-serve handlers/tests and README.

5) Run tests and lint:

    cd go-lcpd
    go test ./...
    golangci-lint run -c .golangci.yml ./...

    cd ../apps/openai-serve
    go test ./...
    golangci-lint run $(go list -f '{{.Dir}}' ./...)

## Validation and Acceptance

Behavioral acceptance (must hold):

- `apps/openai-serve` accepts a `POST /v1/chat/completions` request containing fields like `tools`, `tool_choice`, and `response_format` without rejecting based on unknown fields.
- `stream:true` is rejected with HTTP 400 and an OpenAI-style error JSON.
- The raw HTTP request body bytes are sent over LCP input stream as `application/json; charset=utf-8` / `identity`.
- The Provider returns the raw OpenAI-compatible response body bytes over the LCP result stream as `application/json; charset=utf-8` / `identity`.
- `apps/openai-serve` returns those raw bytes as the HTTP response body (without re-encoding) and sets the HTTP `Content-Type` accordingly.
- Provider discovery advertises supported model IDs in `supported_tasks`, and `openai-serve` uses those to validate `model` when possible.

Test acceptance:

- `apps/openai-serve/internal/httpapi/server_test.go` is updated so that:
  - unknown fields are accepted (previously a strict-JSON 400 test must be inverted),
  - a request containing `tools` is accepted,
  - `stream:true` returns 400,
  - the gRPC `RequestQuote` task is the new `openai_chat_completions_v1` spec containing the raw request JSON bytes.

## Idempotence and Recovery

Regeneration and sync are safe to repeat:

- Re-running `cd go-lcpd && make gen` is idempotent and should only update `go-lcpd/gen/` if protobuf definitions changed.
- Re-copying the generated protobuf Go files into `apps/openai-serve/gen/` is idempotent.

If protobuf regeneration fails:

- Ensure `buf` is installed and you have network access to fetch buf.build remote plugins (or use the Nix devshell to pin tool versions). In this repo, `go-lcpd/Makefile` can re-exec via `nix develop` to pin tool versions; if you have Nix installed, `make gen` is the preferred path.

## Artifacts and Notes

Protocol identifiers introduced by this plan:

- New task kind string: `openai.chat_completions.v1`
- Input stream metadata (MVP): `content_type="application/json; charset=utf-8"`, `content_encoding="identity"`
- Result stream metadata (MVP): `content_type="application/json; charset=utf-8"`, `content_encoding="identity"`

Future streaming compatibility (not implemented in MVP):

- If/when `stream:true` is supported, the result stream bytes must be the raw OpenAI SSE body (`text/event-stream; charset=utf-8`) transported as-is.

Plan revision note (2026-01-02 05:52Z): replaced the prior checklist-style draft with an ExecPlan document to comply with `.agent/PLANS.md` and the repository’s ExecPlans workflow.

Plan revision note (2026-01-02 21:40Z): completed the initial survey + docs/proto scaffolding steps, updated the plan to match the chosen protobuf shapes (`OpenAIChatCompletionsV1*` messages) and the model discovery representation (one supported_tasks template per model), and documented the Japanese protocol doc’s scope.

Plan revision note (2026-01-02 23:47Z): implemented the `apps/openai-serve` passthrough gateway changes (raw request/response bytes, unknown fields allowed, `stream:true` rejected), updated tests/docs, and validated with module tests + golangci-lint.

## Interfaces and Dependencies

Wire protocol definitions (docs-only):

- `task_kind="openai.chat_completions.v1"` defines how to interpret the LCP input/result streams as OpenAI Chat Completions HTTP body bytes.
- `supported_tasks` is used for discovery; the Provider advertises a non-empty supported model list for this task kind.

Go interfaces that must exist at the end:

- In `go-lcpd/internal/lcptasks/lcptasks.go`, extend:
  - `ValidateTask(*lcpdv1.Task) error` to accept the new task kind.
  - `ToWireQuoteRequestTask(*lcpdv1.Task) (QuoteRequestTask, error)` to emit `TaskKind="openai.chat_completions.v1"`.
  - `ToWireInputStream(*lcpdv1.Task) (InputStream, error)` to emit JSON content metadata and decoded bytes equal to the request body.

- In `go-lcpd/internal/provider`, extend validation/handling to accept the new task kind and to execute by proxying bytes to an OpenAI-compatible upstream.

Libraries:

- Prefer `net/http` for passthrough proxying so unknown JSON fields are preserved.
- Continue using existing repository dependencies (Buf/protovalidate for protobuf validation and existing LCP wire/TLV helpers).
