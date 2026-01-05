# Add OpenAI Responses API + streaming passthrough over LCP v0.2

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This repository defines ExecPlan requirements in `.codex/skills/lcp-execplan/references/PLANS.md`. Maintain this document in accordance with that file.

## Purpose / Big Picture

OpenAI 互換クライアントが `apps/openai-serve/` に対して以下の API を使えるようにする:

1) `POST /v1/chat/completions`（Chat Completions API）
2) `POST /v1/responses`（Responses API）

さらに、両方の API について `stream` の有無で次を選べるようにする:

- 非ストリーミング: 通常の JSON レスポンス body を一括で返す
- ストリーミング: `text/event-stream`（SSE）を HTTP で逐次返す

本変更の中核は「HTTP の request/response body bytes を、LCP v0.2 の input/result stream の decoded bytes としてそのまま搬送する」こと。ゲートウェイ（`openai-serve`）も Provider も、SSE の event 内容を解釈/再エンコードせず、bytes を中継する。

利用者が成功を確認する方法は、`curl -N` で SSE が逐次流れることを観測すること（体感として “レスポンスが途中から見える” 状態になる）。

## Progress

- [x] (2026-01-04 23:44Z) 現状調査（chat completions の非ストリーミング実装、LCP stream 仕様、必要な変更点）と、本 ExecPlan の作成を完了。
- [x] (2026-01-05 00:08Z) `docs/protocol/protocol.md` に `task_kind="openai.responses.v1"` と streaming 仕様（chat + responses）を追記（日本語概要 `docs/protocol/protocol-ja.md` も追随）。
- [x] (2026-01-05 00:08Z) `go-lcpd/proto/lcpd/v1/lcpd.proto` を拡張し、Responses 用 TaskSpec と server-streaming RPC を追加（`go-lcpd/gen/go/` と `proto-go/` を再生成）。
- [x] (2026-01-05 02:10Z) `go-lcpd/` で Requester/Provider の streaming 経路（Waiter → gRPC → openai-serve）を実装し、テストで観測可能にした（regtest itest で検証）。
- [x] (2026-01-05 02:10Z) `apps/openai-serve/` に `POST /v1/responses` と、両 endpoint の `stream:true` SSE 中継を実装。
- [x] (2026-01-05 02:15Z) regtest itest を追加し、`stream`/`responses` の end-to-end が確実に通ることを確認（`cd go-lcpd && make test-regtest-openai-serve`）。
- [x] (2026-01-05 03:44Z) `go test` と lint を通す（`go-lcpd` / `apps/openai-serve`）。（completed: `go-lcpd: make test lint test-regtest`; `apps/openai-serve: make test lint`）

## Surprises & Discoveries

- Observation: LCP v0.2 の result stream は `lcp_stream_begin(stream_kind=result)` で `total_len` と `sha256` を省略できる（未知の場合）。ただし `lcp_stream_end` では必須。
  Evidence: `docs/protocol/protocol.md` の “Result stream (`stream_kind = result`)” 記述。

- Observation: `proto-go/` の生成は `go-lcpd` の Buf module 設定を使い、`buf generate --template ../proto-go/buf.gen.yaml` で更新する。
  Evidence: `proto-go/README.md`。

- Observation: gRPC API に新しい server-streaming RPC を追加すると、`apps/openai-serve` のテスト用 mock が `LCPDServiceClient` を満たさずビルドが落ちる。
  Evidence: `apps/openai-serve/internal/httpapi/server_test.go` の `recordingLCPDClient` に `AcceptAndExecuteStream` を追加して修正。

- Observation: `lcp_manifest` は “送信成功” でも相手側が `SubscribeCustomMessages` 未確立だと取りこぼす可能性があり、片側だけ LCP-ready になる（待ちが 3 分でタイムアウトする）ケースがある。
  Fix: “最初の inbound manifest を観測したら（既に送信済みでも）一度だけ manifest を返信する” ことで handshake を自己修復する。

## Decision Log

- Decision: `openai.chat_completions.v1` は既存 task_kind を維持し、`request_json.stream=true` を許可して SSE bytes passthrough を行う。
  Rationale: 既存の routing / supported_tasks / pricing の骨格を崩さず streaming 対応できる。
  Date/Author: 2026-01-04 (Codex)

- Decision: Responses API は新しい `task_kind="openai.responses.v1"` として追加し、request/response bytes passthrough を行う。
  Rationale: Chat Completions と Responses は endpoint/意味論が異なるため、LCP task_kind で明確に分離する。
  Date/Author: 2026-01-04 (Codex)

- Decision: `stream:true` のとき、SSE の内容（event 名や JSON）を解釈しない。Provider と openai-serve は bytes を逐次中継する。
  Rationale: OpenAI 互換ストリーム形式は進化が速く、パース/再エンコードは互換性を壊しやすい。
  Date/Author: 2026-01-04 (Codex)

- Decision: openai-serve が HTTP streaming を行うために、`lcpd-grpcd` の gRPC API に server-streaming RPC を追加する。
  Rationale: 既存の `AcceptAndExecute` は完了まで待って bytes を一括返却するため、HTTP streaming を実現できない。
  Date/Author: 2026-01-04 (Codex)

- Decision: streaming の HTTP 応答は checksum 検証完了前から bytes を書き始める。終端で mismatch が発覚した場合は接続をエラー終了し、ログに記録する。
  Rationale: streaming の価値は “先に見えること”。巻き戻しは不可能なので、最終検証失敗は接続エラーとして扱う。
  Date/Author: 2026-01-04 (Codex)

- Decision: Provider は同一 model に対して `openai.chat_completions.v1` と `openai.responses.v1` の両方を `supported_tasks` として広告する（可能な限り）。
  Rationale: openai-serve の model 検証/peer 選択を単純化し、「片方だけ対応」の事故を減らす。
  Date/Author: 2026-01-04 (Codex)

## Outcomes & Retrospective

この ExecPlan が完了した時点で、以下が成立している:

- `apps/openai-serve` が `POST /v1/chat/completions` と `POST /v1/responses` の両方を提供し、両方とも `stream` の有無で非ストリーミング/ストリーミングが選べる。
- LCP では次が成立する:
  - input stream `content_type="application/json; charset=utf-8"`, `content_encoding="identity"`
  - result stream:
    - 非ストリーミング: `content_type="application/json; charset=utf-8"`
    - ストリーミング: `content_type="text/event-stream; charset=utf-8"`
- `go-lcpd` と `apps/openai-serve` の unit tests と lint が通る。
- ログに raw prompt（`messages[].content` / `input` など）や raw model output（JSON/SSE 本体）を出さない。

## Context and Orientation

この ExecPlan における用語:

- “Chat Completions API”: `POST /v1/chat/completions`。request JSON は `model` と `messages` を持つ。
- “Responses API”: `POST /v1/responses`。request JSON は通常 `model` と `input` を持つ（input は string または array を許容する）。
- “ストリーミング”: request JSON の `stream:true` により、HTTP レスポンスが `text/event-stream`（SSE）で逐次返る状態。
- “passthrough”: JSON を構造体にデコードして再エンコードせず、HTTP request/response body bytes をそのまま運ぶこと。

現状の実装（出発点）:

- `apps/openai-serve` は `POST /v1/chat/completions` をサポート（非ストリーミングのみ）し、HTTP body bytes を `openai.chat_completions.v1` として LCP へ転送する。
  - `apps/openai-serve/internal/httpapi/handler_chat.go` は `stream:true` を 400 で拒否している。
  - `apps/openai-serve/internal/httpapi/routing.go` は `supported_tasks` を見て model → peer を決める（現状は chat completions の task kind のみ）。

- `go-lcpd` は Provider/Requester の両モードを持つ。
  - Provider は upstream OpenAI 互換 HTTP（`/chat/completions`）へ proxy し、結果 body bytes を LCP result stream で返す（非ストリーミング read）。
  - Requester は結果 stream を再構築してから gRPC `Result.result` に入れて返す（非ストリーミング）。

重要な制約:

- `apps/openai-serve` / `go-lcpd` はログを機微情報として扱う。raw prompt / raw output をログに出してはいけない。
- LCP v0.2 では stream は begin/chunk/end により bytes を運ぶ。result stream の begin は `total_len/sha256` を省略できるが、end では必須。

## Plan of Work

作業は「仕様 → 型（proto）→ go-lcpd（Requester/Provider）→ openai-serve（HTTP）」の順で行う。理由は、HTTP 側の streaming 実装は gRPC の streaming RPC が先に必要で、gRPC streaming は LCP result stream chunk を逐次受け取れる実装が先に必要だから。

### 1) プロトコル仕様（docs）

`docs/protocol/protocol.md` に、標準 task_kind として `openai.responses.v1` を追加する。加えて、`openai.chat_completions.v1` と `openai.responses.v1` の両方について `stream:true` の意味論を明文化する:

- input stream bytes: 対応する HTTP request body bytes（JSON）
- result stream bytes:
  - 非ストリーミング: HTTP response body bytes（JSON）
  - ストリーミング: HTTP response body bytes（SSE, `text/event-stream`）

### 2) proto / gRPC API（Requester が streaming できる形）

`go-lcpd/proto/lcpd/v1/lcpd.proto` に以下を追加する:

- `LCPTaskKind` に `LCP_TASK_KIND_OPENAI_RESPONSES_V1`（文字列は `"openai.responses.v1"`）
- `Task` に `openai_responses_v1`（`bytes request_json` と `params.model` を必須）
- `LCPTaskTemplate` に `openai_responses_v1`（supported_tasks 用）
- `AcceptAndExecuteStream` のような server-streaming RPC
  - begin（content_type/encoding）, chunk（bytes）, terminal（Result）を送れる oneof event を定義する

この変更により `apps/openai-serve` は “非ストリーミングなら既存 RPC、ストリーミングなら新 RPC” を選べるようになる。

### 3) go-lcpd（Requester 側）

Requester（`lcpd-grpcd`）は LCP result stream を再構築する現在の `requesterwait.Waiter` に “逐次購読” を追加する必要がある。具体的には:

- `go-lcpd/internal/requesterwait/waiter.go` に “result stream begin/chunk/end/terminal” をイベントとして流す経路を追加する。
- `go-lcpd/internal/grpcservice/lcpd/service.go` に `AcceptAndExecuteStream` を実装する。
  - invoice verify/pay までは既存と同じ。
  - result stream begin/chunk を受け取ったら gRPC stream に forward する。
  - `lcp_result` を受け取ったら terminal event を送り、RPC を正常終了する。

### 4) go-lcpd（Provider 側 + compute backend）

Provider は upstream OpenAI 互換 HTTP からの非ストリーミング/ストリーミングを両方扱う必要がある。方針:

- 非ストリーミングは既存通り “全 body を読み切って output bytes を返し、Provider が一括で result stream を送る”。
- ストリーミングは “upstream の body を reader で読みながら、Provider が result stream chunk を逐次送る”。
  - `lcp_stream_begin(stream_kind=result)` では `total_len/sha256` を省略し、終端で `lcp_stream_end(total_len, sha256)` を送る。

これを実現するために:

- `go-lcpd/internal/computebackend` に streaming 用のインターフェイス（例: `StreamingBackend`）を追加する。
- `go-lcpd/internal/computebackend/openai/backend.go` を拡張し、次をサポートする:
  - `openai.chat_completions.v1`（非ストリーミング/ストリーミング）
  - `openai.responses.v1`（非ストリーミング/ストリーミング）
- `go-lcpd/internal/provider/handler.go` の実行部を、backend が streaming を提供する場合に streaming 経路へ分岐させる。
- `go-lcpd/internal/llm/estimator.go` は task kind チェックが `openai.chat_completions.v1` 固定なので、`openai.responses.v1` も許可する（pricing が落ちるのを防ぐ）。
- Provider の supported_tasks 生成（`go-lcpd/tools/lcpd-grpcd/main.go`）と validator（`go-lcpd/internal/provider/validation.go`）を `openai.responses.v1` に対応させる。

### 5) openai-serve（HTTP）

`apps/openai-serve` は endpoint を 2 つ提供し、どちらも `stream` によって “非ストリーミング” と “ストリーミング” を切り替える:

- `POST /v1/chat/completions`:
  - `stream` omitted/false: 既存通り `AcceptAndExecute` で一括 bytes を返す。
  - `stream:true`: `AcceptAndExecuteStream` を使って SSE bytes を逐次 `c.Writer` に書き込む。
- `POST /v1/responses`:
  - handler を新設し、同様に `stream` の有無で分岐する。

ルーティングは `supported_tasks` の task kind（chat/responses）を見て peer を選べる必要があるため、`apps/openai-serve/internal/httpapi/routing.go` を “task kind を引数に取る” 形に拡張する。

## Concrete Steps

この repo は multi-module Go。コマンドは各 module ディレクトリで実行する。

1) docs 更新

   - `docs/protocol/protocol.md` を編集して `openai.responses.v1` と streaming の意味論を追加する。
   - `apps/openai-serve/README.md` と `docs/openai-serve/overview.mdx` を更新し、`/v1/responses` と `stream:true` の利用例（`curl -N`）を追加する。

2) proto 更新と再生成

   - `go-lcpd/proto/lcpd/v1/lcpd.proto` を更新する。
   - 生成コードを更新する:

       cd go-lcpd
       make gen
       buf generate --template ../proto-go/buf.gen.yaml

3) go-lcpd 実装

   - `go-lcpd/internal/lcpwire`: `OpenAIResponsesV1Params` TLV の encode/decode を追加する（model TLV type=1 を踏襲）。
   - `go-lcpd/internal/lcptasks`: `openai.responses.v1` TaskSpec の validate/to-wire を追加し、`openai.chat_completions.v1` の `stream:true` を許可する。
   - `go-lcpd/internal/provider`: Responses task kind 対応、streaming 実行、result stream の streaming 送信に対応。
   - `go-lcpd/internal/requesterwait`: result stream の逐次イベント購読に対応。
   - `go-lcpd/internal/grpcservice/lcpd`: `AcceptAndExecuteStream` を追加。
   - `go-lcpd/internal/llm/estimator.go`: `openai.responses.v1` を許可。

4) openai-serve 実装

   - `apps/openai-serve/internal/httpapi/server.go`: `POST /v1/responses` を追加。
   - `apps/openai-serve/internal/httpapi/handler_chat.go`: `stream:true` を許可し streaming RPC を使う分岐を実装。
   - `apps/openai-serve/internal/httpapi/handler_responses.go`: 新規追加。
   - `apps/openai-serve/internal/httpapi/server_test.go`: 両 endpoint の streaming/non-streaming テストを追加。

5) テストと lint

   - `cd go-lcpd && make test lint`
   - `cd apps/openai-serve && make test lint`

## Validation and Acceptance

HTTP 動作確認（手動）:

- `POST /v1/chat/completions`（ストリーミング）:

      curl -N http://127.0.0.1:8080/v1/chat/completions \
        -H 'content-type: application/json' \
        -H 'authorization: Bearer devkey1' \
        -d '{"model":"gpt-5.2","stream":true,"messages":[{"role":"user","content":"Say hello."}]}'

  期待: `data:` 等を含む SSE bytes が逐次出力される（内容は passthrough のため固定しない）。

- `POST /v1/responses`（ストリーミング）:

      curl -N http://127.0.0.1:8080/v1/responses \
        -H 'content-type: application/json' \
        -H 'authorization: Bearer devkey1' \
        -d '{"model":"gpt-5.2","stream":true,"input":"Say hello."}'

  期待: SSE bytes が逐次出力される。

非ストリーミングの受け入れ条件:

- `stream` omitted/false のとき、両 endpoint は HTTP 200 + JSON body bytes を一括で返す（既存互換）。

テスト受け入れ条件（回帰防止）:

- `apps/openai-serve/internal/httpapi/server_test.go` に以下がある:
  - chat completions: `stream:true` が 400 にならず、gRPC streaming RPC が呼ばれること。
  - responses: 非ストリーミング/ストリーミングの両方で 200 が返り、task spec が期待通りであること。
- regtest itest として `go-lcpd/itest/e2e/regtest_openai_serve_test.go` があり、実プロセス（`lcpd-grpcd` + `openai-serve`）で end-to-end が通ること（`cd go-lcpd && make test-regtest-openai-serve`）。
  - デバッグ用: `LCP_ITEST_TEE=1` を付けるとプロセス出力（stdout/stderr）を tee できる。

安全性:

- openai-serve/go-lcpd のログに raw prompt / raw output（JSON/SSE 本体）を出さないこと。

## Idempotence and Recovery

- protobuf 再生成は反復実行してよい:
  - `cd go-lcpd && make gen`
  - `cd go-lcpd && buf generate --template ../proto-go/buf.gen.yaml`

- streaming が “途中で切れる” 場合の切り分け:
  - `OPENAI_SERVE_TIMEOUT_EXECUTE` と HTTP server の `WriteTimeout`（`cmd/openai-serve/main.go`）が短すぎないか確認する。
  - LCP の `max_stream_bytes`（manifest）が小さすぎないか確認する。

## Artifacts and Notes

wire-level の期待値（実装の共通理解）:

- `task_kind="openai.chat_completions.v1"`:
  - input bytes: `POST /v1/chat/completions` request body bytes（JSON）
  - result bytes:
    - 非ストリーミング: response body bytes（JSON）
    - ストリーミング: response body bytes（SSE）

- `task_kind="openai.responses.v1"`:
  - input bytes: `POST /v1/responses` request body bytes（JSON）
  - result bytes:
    - 非ストリーミング: response body bytes（JSON）
    - ストリーミング: response body bytes（SSE）

## Interfaces and Dependencies

完成時に存在するべき主要インターフェイス（高レベル）:

- `go-lcpd/proto/lcpd/v1/lcpd.proto`:
  - `LCP_TASK_KIND_OPENAI_RESPONSES_V1`
  - `OpenAIResponsesV1TaskSpec` / `OpenAIResponsesV1Params`
  - `rpc AcceptAndExecuteStream(...) returns (stream ...)`

- `go-lcpd/internal/computebackend`:
  - streaming を表現する追加インターフェイス（例: `StreamingBackend`）
  - Provider は `stream:true` のときに reader を読みながら LCP result chunk を送れること

- `apps/openai-serve/internal/httpapi`:
  - `/v1/chat/completions` と `/v1/responses` の両方で `stream` の値によって gRPC 非ストリーミング/ストリーミングを切り替えること

## Plan Maintenance Notes

(2026-01-05 00:08Z) `openai.responses.v1` と streaming の protocol spec 追記、および `lcpd.proto` の拡張と生成コード更新を反映するために `Progress` と `Surprises & Discoveries` を更新。
