# 設定（概要）

このドキュメントは「何を」「どこで」設定するかを俯瞰します。

## 最小構成

- Requester のみ: `LCPD_LND_*`（ピアメッセージング / 請求書支払い）を設定して `lcpd-grpcd` を起動
- Provider: 追加で `LCPD_PROVIDER_CONFIG_PATH`（YAML）と `LCPD_BACKEND`（例: `openai`）を設定

## 環境変数（必須/任意）

### go-lcpd（共通）

| 変数                                      |                              必須 | 目的                                                       |
| ----------------------------------------- | --------------------------------: | ---------------------------------------------------------- |
| `LCPD_LOG_LEVEL`                          |                              任意 | `debug`/`info`/`warn` など                                 |
| `LCPD_BACKEND`                            | Provider では実質必須（有効化時） | `openai` / `deterministic` / `disabled`                    |
| `LCPD_OPENAI_API_KEY` or `OPENAI_API_KEY` | `LCPD_BACKEND=openai` で必須      | OpenAI API key                                             |
| `LCPD_OPENAI_BASE_URL`                    |                              任意 | OpenAI 互換 API の Base URL                                |
| `LCPD_DETERMINISTIC_OUTPUT_BASE64`        |                              任意 | `deterministic` backend の固定出力（開発/テスト用）         |

### lnd 連携（ピアメッセージング / 支払い）

| 変数                                |                必須 | 目的                                                                    |
| ----------------------------------- | ------------------: | ----------------------------------------------------------------------- |
| `LCPD_LND_RPC_ADDR`                 | lnd を使うなら必須  | `host:port`                                                             |
| `LCPD_LND_TLS_CERT_PATH`            |      通常は必須     | `tls.cert` パス（空ならシステムルートで検証）                            |
| `LCPD_LND_ADMIN_MACAROON_PATH`      | mainnet では推奨     | admin macaroon（支払い / invoice 操作に必要）                            |
| `LCPD_LND_MANIFEST_RESEND_INTERVAL` |                任意 | `lcp_manifest` 再送間隔（例: `30s`。`0s` で無効化）                      |

典型的なデフォルトパス（ネットワークと lnd 構成に合わせて調整してください）:

```sh
export LCPD_LND_RPC_ADDR="localhost:10009"
export LCPD_LND_TLS_CERT_PATH="$HOME/.lnd/tls.cert"
export LCPD_LND_ADMIN_MACAROON_PATH="$HOME/.lnd/data/chain/bitcoin/mainnet/admin.macaroon"
```

### ログ（プライバシー）

`lcpd-grpcd` のログは、生のユーザー入力を永続化しなくても診断できるように設計しています。

- `LCPD_LOG_LEVEL` で詳細度を制御します（`debug` / `info` / `warn` / `error`。デフォルトは `info`）。
- ログには、生のプロンプト / 生のモデル出力 / API key / macaroon / BOLT11 invoice を残してはいけません。
- 生データを残さなくても、ログにはメタデータ（peer id / job id / 価格 / 時間など）が残ります。

詳細: [ログとプライバシー](/go-lcpd/docs/logging-ja)。

### Provider（YAML）

| 変数                        |        必須 | 目的                                                                                                   |
| --------------------------- | ----------: | ------------------------------------------------------------------------------------------------------ |
| `LCPD_PROVIDER_CONFIG_PATH` | Provider で必須 | Provider の YAML 設定ファイルのパス（未指定の場合、カレントに `config.yaml` があればそれを使用）         |

Provider の設定は YAML 優先（`LCPD_PROVIDER_CONFIG_PATH`）です。YAML がない場合、Provider モードは無効です。

Provider YAML の詳細と例は下の「Provider 設定（YAML）」で説明します。

## Provider 設定（YAML）

`lcpd-grpcd` の Provider 振る舞いは YAML のみで設定します。

- パスは `LCPD_PROVIDER_CONFIG_PATH` で渡します
- 未指定の場合、カレントディレクトリの `config.yaml` を使用します（存在する場合）
- ファイルが空/欠落している場合はデフォルトが適用されます（Provider 無効、TTL=300s、`max_output_tokens=4096`、内蔵の価格表など）

明示的にデフォルトを書いたサンプル: `../config.yaml`

### 例

#### mainnet 例

```yaml
enabled: true
quote_ttl_seconds: 600

pricing:
  # 任意: 負荷（in-flight job 数）に応じた surge pricing（quote 時点でのみ適用）
  # multiplier = 1.0 + per_job_bps/10_000 * max(0, in_flight_jobs - threshold)
  in_flight_surge:
    threshold: 2
    per_job_bps: 500 # threshold を超えた 1 job あたり +5%
    max_multiplier_bps: 30000 # 3.0x 上限

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

#### regtest 例

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

### モデル / プロファイル命名

- `profile` は LCP wire の識別子（`llm_chat_params.profile`）です。以下に出現します:
  - `lcp_manifest.supported_tasks[].llm_chat.profile`（広告）
  - Provider 側の pricing lookup（invoice / terms binding）
- `backend_model` は compute backend に渡す上流モデル ID（例: OpenAI 互換の `model`）です。
- `profile` と `backend_model` は一致する必要はありません。`llm.chat_profiles.*.backend_model` でマッピングします。

### フィールドリファレンス

- `enabled`: Provider モードを有効化します。`false` の場合、quote/cancel を拒否し、invoice を作りません。
- `quote_ttl_seconds`: Quote と invoice の TTL（秒）。デフォルト 300s。
- `pricing.in_flight_surge`: 任意の surge pricing。quote 時点の in-flight job 数に応じて価格を倍率調整します。
  - `threshold`: surge を開始する in-flight job 数の閾値。
  - `per_job_bps`: `threshold` を超えた 1 job あたりの加算倍率（bps。10,000 = 1.0x）。`0` の場合は無効。
  - `max_multiplier_bps`: 総倍率の上限（bps）。`0` の場合は安全なデフォルト上限が使われます。
- `llm.max_output_tokens`: Provider 全体の execution policy（`ExecutionPolicy`）のデフォルト。quote 時の推定と backend 実行の両方に適用します。デフォルト 4096。
- `llm.chat_profiles`: 許可/広告する `llm.chat` プロファイルのマップ。空の場合、任意プロファイルを受け付けますが manifest では広告しません。
  - `backend_model`: backend に渡す上流モデル ID。デフォルトはプロファイル名。
  - `max_output_tokens`: 任意のプロファイルごとの上書き（0 より大きいこと）。
  - `price`: プロファイルごとの価格（msat / 100 万トークン）。`input_msat_per_mtok` と `output_msat_per_mtok` は必須、`cached_input_msat_per_mtok` は任意。
  - `openai`: 任意の OpenAI 互換 Chat Completions パラメータ（`temperature`、`top_p`、`stop`、`presence_penalty`、`frequency_penalty`、`seed`）。

### デフォルト価格表

YAML がない場合、内蔵の価格表（msat / 100 万トークン）を使用します:
- `gpt-5.2`: input 1,750,000 / cached 175,000 / output 14,000,000

### Quote → Execute フロー（`llm.chat`）

1. QuoteRequest を検証し、プロファイルが許可されているか確認します。
2. `computebackend.Task` に ExecutionPolicy（`max_output_tokens`）を適用します。
3. `UsageEstimator`（`approx.v1`: `ceil(len(bytes)/4)`）でトークン使用量を推定します。
4. `QuotePrice(profile, estimate, cached=0, price_table)` で msat 価格を計算し、任意で `pricing.in_flight_surge` を適用してから TermsHash / invoice binding に埋め込みます。
5. 支払いが確定したら、`profile -> backend_model` を解決し、backend でタスクを実行して `lcp_result` で返します。

## backend に関する補足

- `openai`: 外部 API を呼び出します（課金 / レート制限 / ネットワーク依存）。
  - OpenAI 互換 Chat Completions API（`POST /v1/chat/completions`）を使用します。
  - `llm.chat` の `profile` は `llm.chat_profiles.*.backend_model` で上流 `model` にマッピングします（省略時は profile 名）。
  - `params_bytes` JSON をサポートします:
    - `max_output_tokens`（および旧 `max_tokens`）を `max_completion_tokens` として送信
    - `temperature`、`top_p`、`stop`、`presence_penalty`、`frequency_penalty`、`seed`
  - 単一の text user message を送信します（tools なし / multimodal なし / streaming なし）。
- `deterministic`: 開発用の固定出力 backend（外部 API なし）。
- `disabled`: 実行しません（Requester のみ運用で便利）。

## 関連ドキュメント

- gRPC 開発 CLI: `cli-ja.md`
- mainnet 手順: `quickstart-mainnet-ja.md`
- regtest 手順: `regtest-ja.md`
