# LCP v0.1 仕様（日本語概要）

このページは LCP v0.1 の日本語概要です。
正規（normative）な仕様は英語版の `docs/protocol/protocol.md` を参照してください（このリポジトリでは英語版が SSOT です）。

## 概要

LCP（Lightning Compute Protocol）は、Lightning の既存トランスポート（BOLT #1 カスタムメッセージ）上で動作するアプリケーション層プロトコルです。
Quote → Pay → Result の流れで、計算ジョブに対する支払いと結果受け渡しを行います。

- BOLT（Lightning 仕様）を変更しません
- 直接の Lightning ピア接続（Requester と Provider の peering）が必要です
- 支払い（BOLT11 invoice）とジョブ条件（Terms）を `terms_hash` でバインドし、invoice swapping を防ぎます

## ゴール（抜粋）

LCP v0.1 は以下を定義します:

- ジョブ交渉のための最小 P2P メッセージセット
- 支払いとジョブ条件のバインディング（invoice swapping 対策）
- 結果を 1 メッセージで返す仕組み
- アプリケーション層の idempotency / リプレイ耐性
- Provider が受理するタスクテンプレート（`task_kind` + `params`）を接続スコープで宣言する方法

## 基本フロー（概念）

1. Requester → Provider: `lcp_quote_request`
2. Provider → Requester: `lcp_quote_response`（`price_msat` / `terms_hash` / BOLT11 invoice 等）
3. Requester が invoice を支払う（`description_hash == terms_hash` 等を検証）
4. Provider → Requester: `lcp_result`

## メッセージ種別（v0.1）

LCP v0.1 は BOLT #1 の odd type のカスタムメッセージを使います（unknown odd は ignore / unknown even は fatal のルールに準拠）。

- 42081: `lcp_manifest`
- 42083: `lcp_quote_request`
- 42085: `lcp_quote_response`
- 42087: `lcp_result`
- 42095: `lcp_cancel`
- 42097: `lcp_error`

## TLV と拡張性（要点）

各メッセージの payload は TLV stream（`bigsize(type)` → `bigsize(length)` → `value`）です。

- TLV は type 昇順にソートします
- 同一 type の重複はしません
- unknown TLV は無視します（forward compatibility）

## LCP v0.2 の `task_kind`: `openai.chat_completions.v1`（概要）

LCP v0.2 では `task_kind="openai.chat_completions.v1"` を定義し、OpenAI 互換の `POST /v1/chat/completions` の HTTP body（JSON）を LCP の input/result stream の bytes としてそのまま運びます。

- input stream: `content_type="application/json; charset=utf-8"`, `content_encoding="identity"`
- `params` は TLV stream（`openai_chat_completions_v1_params_tlvs`）で、少なくとも `model` を含みます
- result stream（non-streaming）: `content_type="application/json; charset=utf-8"`, `content_encoding="identity"`

詳細・正規（normative）な仕様は英語版 `docs/protocol/protocol.md` の §5.2.1 を参照してください。

## 参考

- LCP 英語仕様（SSOT）: `docs/protocol/protocol.md`
- Lightning BOLT #1 messaging: `https://github.com/lightning/bolts/blob/master/01-messaging.md`
