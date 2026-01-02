# go-lcpd 実装仕様（日本語概要）

このページは `go-lcpd/spec.md` の日本語概要です。
正規（normative）な実装仕様は英語版の `go-lcpd/spec.md` を参照してください（このリポジトリでは英語版が SSOT です）。

## これは何？

`go-lcpd` は LCP（Lightning Compute Protocol）の参照実装（daemon）です。
Lightning の BOLT #1 カスタムメッセージを `lnd` のピア接続上でやり取りし、Requester 側のフロー（quote → pay → result）をローカル gRPC API で操作できるようにします。

## 主要ポイント（抜粋）

- 単一の真実（SSOT）:
  - gRPC API: `go-lcpd/proto/lcpd/v1/lcpd.proto`
  - wire protocol: `protocol/protocol.md`（LCP v0.1, `protocol_version=1`）
- transport:
  - 独自 TCP/UDP transport は実装しません（peer messaging は `lnd` に委譲）
- 互換性:
  - unknown odd message を無視、unknown even を切断（BOLT #1 parity）
  - unknown TLV は無視（forward compatibility）
- セキュリティ:
  - 支払い前に invoice の `description_hash == terms_hash` 等を検証
  - quote expiry / envelope expiry の制限と重複排除で replay/DoS を軽減
  - ログには生のプロンプト/出力や秘密情報（API key / macaroon / invoice 文字列など）を残さない
- テスト:
  - Lightning 統合テストは regtest を opt-in で実行（`go test ./...` に外部プロセスを要求しない）

## 参考

- go-lcpd 英語仕様（SSOT）: `go-lcpd/spec.md`
- LCP 英語 wire 仕様（SSOT）: `protocol/protocol.md`
