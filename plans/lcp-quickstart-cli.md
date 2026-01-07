# lcp-quickstart（app）: lnd起動 → チャネル開設 → openai-serve起動（CLIから開始、将来UI対応、testnet/mainnet）

このExecPlanは生きたドキュメントです。実装が進むたびに、`Progress` / `Surprises & Discoveries` / `Decision Log` / `Outcomes & Retrospective` を更新し続けてください。

本リポジトリのExecPlan標準は `/.codex/skills/lcp-execplan/references/PLANS.md` にあります。本書はそれに従って維持します。

## Purpose / Big Picture

開発者が「手元のMac/Linuxで、Lightning（lnd）+ LCP + OpenAI互換HTTPを最短で動かす」ための“ワンコマンド体験”を提供します。最初はCLIツールとして実装し、将来は同じ“中核ロジック”を使ってGUI（デスクトップ/ローカルWeb UI）を載せられる構造にします。

この変更の後、ユーザーは以下を同じアプリで行えます:

- `testnet`: `lcp-quickstart testnet up --provider <pubkey>@<host:port>` で、lndの状態確認 → Provider接続 →（必要なら）チャネル開設案内 → `lcpd-grpcd` 起動 → `openai-serve` 起動までを“一連の体験”として案内/自動化する。
- `mainnet`: `lcp-quickstart mainnet up --i-understand-mainnet` で同様にセットアップし、OpenAI互換HTTPエンドポイントをローカルに立ち上げる（危険操作は明示的な許可が必要）。

動作確認は `curl http://127.0.0.1:8080/healthz` と `curl http://127.0.0.1:8080/v1/chat/completions ...` で行い、「OpenAI互換HTTPが返る」ことを観測できます。

## Progress

- [x] (2026-01-06 05:14Z) 既存の起動手順（openai-serve / quickstart mainnet）を調査し、初期案を作成した。
- [x] (2026-01-06 06:34Z) 独立app化（`apps/lcp-quickstart/`）と将来UI前提（状態管理/イベント）を設計に反映した。
- [x] (2026-01-06 07:21Z) WYSIWID-style spec（Concepts + Synchronizations）を追加した（`apps/lcp-quickstart/spec.md`）。
- [x] (2026-01-06 07:30Z) スコープ変更: regtestを廃止し、testnet/mainnetのみに再設計した（本ExecPlanとspecを更新）。
- [ ] `apps/lcp-quickstart/`（新規Go module）にCLI + 中核ロジック + 状態管理を実装する。
- [ ] `testnet up/status/down/logs/reset` を実装し、最小のセットアップ体験を成立させる。
- [ ] `mainnet up/status/down/logs/reset` を実装し、安全優先で段階的にセットアップできるようにする。
- [ ] 最小ドキュメント（README）を追加し、初心者が同じ結果を再現できるようにする。

## Surprises & Discoveries

- Observation: `openai-serve` は Lightning に直接接続せず、`lcpd-grpcd` gRPC にフォワードするだけのステートレスHTTPゲートウェイである。
  Evidence: `apps/openai-serve/README.md`
- Observation: `go-lcpd/internal/...`（例: `go-lcpd/internal/lnd/lnrpc`）はGoの `internal/` 制約で、`apps/` の別モジュールから直接importできない。
  Evidence: Go `internal` directory rules（構造上の制約）
- Observation: `lncli create` / `lncli unlock` は対話的になりやすく、`create` は seed mnemonic を表示する。
  Evidence: `docs/go-lcpd/docs/quickstart-mainnet.md`
- Observation: このrepo内には “testnet Provider node” の既定値が見当たらない。
  Evidence: `docs/go-lcpd/docs/quickstart-mainnet.md` はmainnetのみ

## Decision Log

- Decision: 実装は「独立したapp」として `apps/lcp-quickstart/` に置く（go-lcpdのtools配下には置かない）。
  Rationale: “ユーザーが使うアプリ”として責務を分離し、将来UI（別entrypoint/別プロセス）を足しやすくするため。
  Date/Author: 2026-01-06 / Codex
- Decision: 将来UIのために、CLIは薄くし、“手順の状態機械”と“状態ファイル（JSON）”を中核にする。
  Rationale: UIは「進捗」「次に必要な操作」を表示したい。print中心設計だと再利用しづらい。
  Date/Author: 2026-01-06 / Codex
- Decision: lnd操作は当面 `lncli` ベースにする。
  Rationale: `apps/` から `go-lcpd/internal/...` を直接importできない。`lncli` はユーザー環境の認証/設定に追従しやすい。
  Date/Author: 2026-01-06 / Codex
- Decision: mainnetの危険操作（チャネル開設など）は明示フラグ（`--i-understand-mainnet` 等）必須にする。
  Rationale: 誤操作による資金損失を防ぐ。
  Date/Author: 2026-01-06 / Codex
- Decision: testnetは `--provider` を必須にする（既定Providerは持たない）。
  Rationale: repo内に既定値がないため。曖昧な推測で接続先を決めない。
  Date/Author: 2026-01-06 / Codex
- Decision: `openai-serve` は常に `OPENAI_SERVE_DEFAULT_PEER_ID` を固定する。
  Rationale: 接続済みの別ピアに誤ルーティングして課金/情報漏えいする事故を防ぐ。
  Date/Author: 2026-01-06 / Codex

## Outcomes & Retrospective

（実装後に記入）

## Context and Orientation

### 用語（このExecPlan内の意味）

- `testnet`: Bitcoin/Lightningのテストネットです。資産価値はありませんが、wallet/seed/運用は本番同様に扱う必要があります。
- `mainnet`: 実在のBTC/Lightningを使うネットワークです。オンチェーン/Lightningで実際に資金が動きます。
- `lnd` / `lncli`: Lightning Network Daemon とそのCLIです。このリポジトリの Lightning 連携は `lnd` に依存します。
- `チャネル（channel）`: Lightningの支払いに必要な“入出力容量”を作るためのオンチェーン取引です。testnet/mainnetではブロック確認が必要です。
- `Provider` / `Requester`:
  - Provider: 計算（LLM等）を提供し、見積（quote）と請求書（invoice）を返し、支払い後に結果を返す側。
  - Requester: Providerへ依頼し、invoiceを支払い、結果を受け取る側。
- `lcpd-grpcd`: LCPのgRPCデーモン（go-lcpdのツール）です。Requesterとして動かし、Lightning peer接続の上でLCPメッセージを交換し、支払いを行います。
- `openai-serve`: OpenAI互換HTTPサーバです。HTTPリクエストを `lcpd-grpcd` に転送し、LCP Providerから得たレスポンスをそのまま返します。

### 既存の関連ファイル/コンポーネント

- `apps/openai-serve/cmd/openai-serve`: OpenAI互換HTTPサーバ本体（Go）。
- `docs/go-lcpd/docs/quickstart-mainnet.md`: mainnet向けの手動quickstart（既定Provider nodeがここにある）。

### UXゴール（このツールが“隠す/短縮する”べき複雑さ）

現状の手動手順では、以下が初心者にとって重い:

- `lnd` の起動と待機
- walletの作成/アンロック（対話的）
- Providerへの接続
- チャネル開設と確認待ち
- `lcpd-grpcd` / `openai-serve` の起動と環境変数設定

本ExecPlanでは「柔軟性よりも簡潔さ」を優先し、まずは `testnet` と `mainnet` の“成功しやすい最短パス”を提供します。

## Plan of Work

### 1) 新app `apps/lcp-quickstart` を追加する（CLIが最初のフロントエンド）

新規ディレクトリ（MVP）:

- `apps/lcp-quickstart/go.mod`（独立Go module）
- `apps/lcp-quickstart/cmd/lcp-quickstart/main.go`（CLI entrypoint）
- `apps/lcp-quickstart/spec.md`（WYSIWID spec: Concepts + Synchronizations。既に追加済み）

将来UIを見据えた内部構造（MVPから用意）:

- `apps/lcp-quickstart/internal/controller/...`（“手順の状態機械”。CLI/UIから再利用）
- `apps/lcp-quickstart/internal/state/...`（状態ファイル `~/.lcp-quickstart/state.json`）
- `apps/lcp-quickstart/internal/proc/...`（外部プロセス起動/停止、ログ/ pid管理）

### 2) コマンド設計（MVP）

最低限のコマンドセット:

- `lcp-quickstart testnet up --provider <pubkey>@<host:port>`
- `lcp-quickstart testnet status`
- `lcp-quickstart testnet logs <component>`
- `lcp-quickstart testnet down`
- `lcp-quickstart testnet reset --force`

- `lcp-quickstart mainnet up --i-understand-mainnet`
- `lcp-quickstart mainnet status`
- `lcp-quickstart mainnet logs <component>`
- `lcp-quickstart mainnet down`
- `lcp-quickstart mainnet reset --force`

共通オプション（MVP）:

- `--provider`: 接続先Provider（`<pubkey>@<host:port>`）。mainnetは未指定なら既定値を使う。
- `--start-lnd`: `lncli getinfo` が通らない時に限り、`lnd` を子プロセスとして起動する（任意）。
- `--open-channel`: outbound liquidity が足りない場合に、`lncli openchannel` まで行う（任意）。
- `--channel-sats`: `--open-channel` で開設するチャネル金額（sats）。未指定ならエラー（誤操作防止）。

mainnet専用の安全フラグ:

- `--i-understand-mainnet`: mainnetで資金が動くセットアップを許可する（必須）。

### 3) 状態管理（UI前提の“最小”）

このappは「今どこまで終わったか」を次で表現できるようにします:

- data dir: `~/.lcp-quickstart/`
- 状態ファイル: `~/.lcp-quickstart/state.json`
- ログ: `~/.lcp-quickstart/logs/<component>.log`
- PID: `~/.lcp-quickstart/pids/<component>.pid`

### 4) `testnet up` の処理フロー（段階的）

`testnet up` は次の順で処理します。途中で失敗した場合は「次に何をすべきか」を明確に表示し、再実行で復旧できるようにします。

1. `lncli --network=testnet getinfo` を試す。
   - 失敗し、かつ `--start-lnd` が指定されていれば `lnd` を起動して再試行する。
2. walletが `uninitialized/locked` の場合は、次を案内して終了（自動化しない）:
   - 初回: `lncli --network=testnet create`
   - ロック時: `lncli --network=testnet unlock`
3. Providerへの接続:
   - `--provider` が必須。
   - `lncli --network=testnet connect <pubkey>@<host:port>`
4. 支払い可能性（チャネル/ルート）確認:
   - 足りない場合は、チャネル開設が必要であることを表示。
   - `--open-channel --channel-sats <N>` が指定されている場合のみ `lncli openchannel` を実行。
5. `lcpd-grpcd`（Requester）を起動:
   - `LCPD_LND_RPC_ADDR` / `LCPD_LND_TLS_CERT_PATH` / `LCPD_LND_ADMIN_MACAROON_PATH` は “標準のデフォルト” を採用しつつ、必要ならフラグで上書きできるようにする。
     - rpc addr: `localhost:10009`
     - tls cert: `~/.lnd/tls.cert`
     - admin macaroon: `~/.lnd/data/chain/bitcoin/testnet/admin.macaroon`
6. `openai-serve` を起動:
   - `OPENAI_SERVE_DEFAULT_PEER_ID` を `--provider` の pubkey に固定する。
   - `OPENAI_SERVE_API_KEYS=lcp-dev` をデフォルトにする。
7. 使い方（base_url / api key / curl例 / 停止コマンド）を表示する。

### 5) `mainnet up` の処理フロー（安全優先・段階的）

mainnetは資金が動くため、必ず `--i-understand-mainnet` を要求します。フローは testnet と同様ですが、以下を追加します:

- Providerは未指定なら既定値を使う（`docs/go-lcpd/docs/quickstart-mainnet.md` の node）。
- `openai-serve` に `OPENAI_SERVE_MAX_PRICE_MSAT` の保守的デフォルトを設定する（ユーザーが上げられる）。

### 6) `status/down/logs/reset` の設計（両network共通）

- `status`: “起動済み/未起動” と “次にやるべきこと” を表示する。`curl /healthz` も成功/失敗を示す。
- `logs`: 対象コンポーネントのログを `tail -f` 相当で追跡する。
- `down`: 起動した順と逆順で停止する（lndは「自分で起動した場合のみ」止める）。
- `reset --force`: appのworkspaceのみ削除する（ユーザーの `~/.lnd` は削除しない）。

## Concrete Steps

### 1) ビルド（1回）

    cd apps/lcp-quickstart
    go install ./cmd/lcp-quickstart

### 2) 起動（testnet）

    lcp-quickstart testnet up --provider "<pubkey>@<host:port>"

### 3) 起動（mainnet）

    lcp-quickstart mainnet up --i-understand-mainnet

### 4) 動作確認（HTTP）

    curl -sS http://127.0.0.1:8080/healthz

期待する出力:

    ok

### 5) 停止

    lcp-quickstart mainnet down

## Validation and Acceptance

受け入れ条件（“観測できる挙動”として定義）:

1. `lcp-quickstart testnet up --provider ...` が、前提が足りない場合に “次にすべきこと” を明確に表示できる（wallet unlock、チャネル開設、など）。
2. `lcp-quickstart mainnet up --i-understand-mainnet` が、同様に段階的に案内できる（危険操作は明示的許可が必要）。
3. `curl http://127.0.0.1:8080/healthz` が `ok\n` を返す。
4. `POST /v1/chat/completions` がHTTP 200を返し、レスポンスボディが空でない（Provider/モデルが有効な場合）。
5. `down` が再実行可能な状態でプロセスを停止する（`status` が stopped を示す）。

## Idempotence and Recovery

- `up` は冪等にする:
  - 既に `lcpd-grpcd` / `openai-serve` が起動済みなら再起動せず `status` を表示して終了してよい。
- `reset --force` は破壊的:
  - appのworkspace（`~/.lcp-quickstart`）のみ削除する。
  - ユーザーの `~/.lnd` は削除しない（絶対に触らない）。

## Artifacts and Notes

（実装後に、実際の `status` 出力例やログの場所を追記）

## Interfaces and Dependencies

### 呼び出す外部プロセス（固定）

- `lncli`（testnet/mainnetのlnd操作）
- `lnd`（`--start-lnd` の場合のみ子プロセスとして起動）
- `go-lcpd/tools/lcpd-grpcd`（ビルドして起動）
- `apps/openai-serve/cmd/openai-serve`（ビルドして起動）

---

Plan revision note (2026-01-06 07:30Z):

ユーザー要望により regtest をスコープから外し、testnet/mainnet のみに絞りました。これに伴い、採掘による自動資金付与やローカルdevnet管理（bitcoind + 2x lnd）は削除し、`lncli` ベースで「段階的に案内しながら進める」設計に切り替えました。
