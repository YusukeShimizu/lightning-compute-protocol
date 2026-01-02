# ログとプライバシー（lcpd-grpcd / go-lcpd）

このプロジェクトではログを **センシティブ** と扱います。  
ログは「何が起きたか（quote → pay → result）」を追えるようにしつつ、ユーザーの生データ（プロンプト等）を永続化しないためのものです。

## 絶対にログに残してはいけないもの（MUST NOT）

- 生のプロンプト（`llm_chat.prompt` / wire の `input`）
- 生のモデル出力（wire の `lcp_result.result`）
- 秘密情報（API key / macaroon / access token など）
- BOLT11 の `payment_request`（invoice 文字列）
- Lightning カスタムメッセージの生 payload や、gRPC のリクエスト/レスポンス全体ダンプ

## ログに残してよい情報（例）

ログは **メタデータ** に寄せます:

- 相関: `job_id`, `peer_id` / `peer_pub_key`
- タスク情報: `task_kind`, `profile`, `prompt_bytes`, `max_output_tokens`, `temperature_milli`
- 見積もり/支払い: `price_msat`, `quote_expiry_unix`
- 時間: `quote_ms`, `pay_ms`, `wait_ms`, `execute_ms`, `total_ms`
- 出力メタデータ: `result_bytes`, `content_type`, `usage_*`（可能な場合の token unit）

## ログレベル

`LCPD_LOG_LEVEL` で出力を制御します（`debug` / `info` / `warn` / `error`）。

- `error`: サービスとして致命的・継続不能な失敗。
- `warn`: ジョブ単位の失敗や異常（ただしプロンプト/出力はログに残さない）。
- `info`: 計測と相関ができるように、ライフサイクルの要点を記録（quote 発行/受領、result 受領、job 完了など）。
- `debug`: 診断向けの追加情報（drop/resend/replay など）。それでも秘密は残さない。

## 運用上の注意

- 生データを残さなくても、ログには **メタデータ**（peer id / job id / 価格 / 時間など）が残ります。保存先・保持期間はセキュリティ上の判断です。
- ログをディスクに保存する場合は、権限の制限とローテーションを推奨します。
  [バックグラウンド実行とログ](/go-lcpd/docs/background-ja) も参照してください。

