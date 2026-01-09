# LCP v0.3 仕様（日本語概要）

このページは LCP v0.3 の日本語概要です。
正規（normative）な仕様は英語版の `docs/protocol/protocol.md` を参照してください（このリポジトリでは英語版が SSOT です）。

## 概要

LCP（Lightning Compute Protocol）は、Lightning の既存トランスポート（BOLT #1 カスタムメッセージ）上で動作するアプリケーション層プロトコルです。
LCP v0.3 は **manifest → call → quote → pay → stream → complete** の流れで、メソッド呼び出し（`method`）の実行と支払い・結果配送を行います。

- BOLT（Lightning 仕様）を変更しません
- Requester と Provider の **直接の Lightning ピア接続**が必要です
- 支払い（BOLT11 invoice）とコール条件（Terms）を `terms_hash` でバインドし、invoice swapping を防ぎます

## 基本フロー（概念）

1. 双方が `lcp_manifest` を交換（必須）
2. Requester → Provider: `lcp_call`（`method` + optional `params`）
3. Requester → Provider: request stream（`lcp_stream_begin/chunk/end`, `stream_kind=request`）
4. Provider → Requester: `lcp_quote`（`price_msat` / `terms_hash` / BOLT11 invoice）
5. Requester が invoice を支払う（`description_hash == terms_hash` 等を検証）
6. Provider → Requester: response stream（`stream_kind=response`）
7. Provider → Requester: `lcp_complete`（終端、必要に応じて response の hash/len 等を含む）

## メッセージ種別（v0.3）

LCP v0.3 は BOLT #1 の odd type のカスタムメッセージを使います（unknown odd は ignore / unknown even は fatal のルールに準拠）。

- 42101: `lcp_manifest`
- 42103: `lcp_call`
- 42105: `lcp_quote`
- 42107: `lcp_complete`
- 42109: `lcp_stream_begin`
- 42111: `lcp_stream_chunk`
- 42113: `lcp_stream_end`
- 42115: `lcp_cancel`
- 42117: `lcp_error`

## TLV と拡張性（要点）

各メッセージの payload は TLV stream（`bigsize(type)` → `bigsize(length)` → `value`）です。

- TLV は type 昇順にソートします
- 同一 type の重複はしません
- unknown TLV は無視します（forward compatibility）

また、全メッセージに `protocol_version`（v0.3 は `3`）が含まれます。`lcp_manifest` 以外の call-scope メッセージは `call_id` / `msg_id` / `expiry` を持ち、受信側はリプレイ防止のために (`call_id`,`msg_id`) で de-duplicate します。

## ストリーム（request/response）

大きな payload は `lcp_stream_*` で分割転送します。

- `stream_kind=1`: request
- `stream_kind=2`: response

チャンクは `seq` が 0 から 1 ずつ増える必要があります。`lcp_stream_end` では decoded bytes の `total_len` と `sha256` を検証します（`sha256` は decoded bytes の SHA256）。

`content_encoding` は少なくとも `identity` をサポートする必要があります。

## `terms_hash` と invoice binding（要点）

Provider は BOLT11 invoice の `description_hash` を `terms_hash` と一致させることで、請求書とコール条件をバインドします。
Requester は支払い前に少なくとも以下を検証します:

- `description_hash == terms_hash`
- invoice の payee pubkey が Provider の peer pubkey と一致
- invoice の金額が `price_msat` と一致（amount-less invoice は不可）
- invoice の expiry が `quote_expiry` を超えない

`terms_hash` 自体は、仕様で定義された `terms_tlvs` の canonical TLV stream を `SHA256` したものです（詳細は英語版仕様）。

## 参考

- LCP 英語仕様（SSOT）: `docs/protocol/protocol.md`
- Lightning BOLT #1 messaging: `https://github.com/lightning/bolts/blob/master/01-messaging.md`
