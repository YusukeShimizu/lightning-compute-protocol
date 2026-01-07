# LDK LCP Node（独立ノード）MVP + go-lcpd 連携

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds.

This plan MUST be maintained in accordance with `.codex/skills/lcp-execplan/references/PLANS.md`.

## Purpose / Big Picture

この計画の目的は、`lnd` を前提とせずに動作する「独立した Lightning ノード（LDK ベース）」を本 repo に追加し、`go-lcpd` がそのノードを利用して LCP（quote → pay → stream(result) → result）を安定的かつ高速に立ち上げられるようにすることです。

ユーザー視点の成果は次の 2 点です。

1) `lnd` を立ち上げなくても、LDK ノード + `go-lcpd/tools/lcpd-grpcd` の組み合わせで LCP を動かせる。

2) 運用に必要な最低限のノード操作（peer 接続、チャネル開設/クローズ、受取 invoice 発行、支払い）をこの LDK ノード単体で完結できる。

「動いていること」の確認は、regtest 上で以下が成立することです。

- 2 台の LDK ノードを起動し、peer 接続とチャネル開設/クローズができる。
- 2 台の `lcpd-grpcd`（Requester/Provider）を起動し、LCP の end-to-end が完走する。
- 起動時間は `lnd` を使う構成より短く、再起動後も state（チャネル、鍵、invoice の待ち受け等）が復元される。

## Progress

- [x] (2026-01-06) 初版 ExecPlan を作成（当初は lnd shim 案）。
- [x] (2026-01-06) 方針転換: “lnd 互換 shim” を作らず、独立ノード + go-lcpd 側の抽象化/変更を許容する方向に更新。
- [ ] (TODO) `go-lcpd` 側に Lightning ノード抽象（実装差し替え可能な interface）を導入する。
- [ ] (TODO) `apps/ldk-lcp-node/`（Rust）を追加し、peer/チャネル/支払い/custom message を提供する。
- [ ] (TODO) `go-lcpd` を LDK ノードに接続して LCP を完走させる（regtest E2E）。

## Surprises & Discoveries

- Observation: `ldk-node` crate（0.7.0 時点）は custom message を外部 API に露出しておらず、LCP の transport（BOLT #1 custom messages）にそのまま使えない。
  Evidence: `ldk-node` の `CustomMessageHandler` は LSPS 等の内部用途向けで、外部へ raw custom message を流す API が見当たらない（crate ソース調査）。

## Decision Log

- Decision: `lnd` の gRPC 互換 shim は作らず、LDK ベースの独立ノードを新設し、`go-lcpd` はそのノード向けの専用 API を使う。
  Rationale: `lnd` 互換 API は設計が `lnd` の都合に引っ張られる。LCP に必要な機能（custom message + 支払い + チャネル管理）だけを中心に API 設計するほうが、依存を最小化し、将来の拡張や置換（完全移行）もしやすい。
  Date/Author: 2026-01-06 / Codex

- Decision: LDK ノードの MVP は “direct channel でしか支払わない” を基本方針とし、マルチホップ支払いは後回しにする。
  Rationale: LCP は custom message のために「直接 peer 接続」が必須である。支払いも direct channel に限定すれば、初期実装のルーティング要件が単純になり、動作確認も容易になる。将来、必要ならマルチホップ支払いを追加する。
  Date/Author: 2026-01-06 / Codex

- Decision: on-chain 同期は Esplora を使い、gossip 同期は Rapid Gossip Sync (RGS) を使う。
  Rationale: 運用上 “bitcoind を自前で立てる/維持する” 依存を減らし、初回同期と再起動の体験を速くする。Esplora は chain 同期の API を簡潔にし、RGS は graph 同期を高速化して将来の拡張（マルチホップ等）にも備えられる。
  Date/Author: 2026-01-06 / Codex

- Decision: 独立ノードは Rust の別プロセス（daemon）として実装し、`go-lcpd` との境界は gRPC にする。
  Rationale: Go↔Rust の in-process FFI はビルド/デバッグ/運用の複雑性が上がる。最初はプロセス境界で責務を明確化し、将来統合する場合も API を保ちやすい。
  Date/Author: 2026-01-06 / Codex

## Outcomes & Retrospective

(未記入)

## Context and Orientation

現状の `go-lcpd` は Lightning ノードを `lnd` gRPC に依存しており、実装は以下に分かれています。

- custom message transport: `go-lcpd/internal/lndpeermsg/*`
- Requester 支払い（decode + pay）: `go-lcpd/internal/lightningrpc/*`
- Provider 請求（invoice create + settle wait）: `go-lcpd/internal/provider/lnd_adapter.go`

一方で、上位ロジックはすでに “小さな interface” に依存しています。

- Requester 側: `go-lcpd/internal/grpcservice/lcpd/service.go` の `PeerMessenger` / `LightningRPC`
- Provider 側: `go-lcpd/internal/provider/handler.go` の `Messenger` / `InvoiceCreator`

この plan では、これらの interface を “lnd 固有実装” から切り離し、LDK ノード実装（gRPC クライアント）を差し込めるようにします。

またユーザー要望として「独立ノードでチャネル開設/クローズも行える」必要があるため、LDK ノード側には運用 API（peer/チャネル/ウォレット）も持たせます。

用語:

- LDK: Rust 製 Lightning Development Kit（`lightning` crate 群）。
- LDK ノード（本計画）: LCP 向けに必要最小限の Lightning ノード機能を持つ新しい daemon。
- direct channel: 相手 peer と自分の間に直接開設されたチャネル（マルチホップではない）。
- custom message: BOLT #1 の custom message（type >= 32768）。LCP v0.2 はこれを transport として利用する。

## Plan of Work

### 目標アーキテクチャ（MVP）

MVP の構成要素は 2 プロセスです。

1) `apps/ldk-lcp-node/`（Rust）: Lightning ノード daemon（peer/チャネル/支払い/custom message）

2) `go-lcpd/tools/lcpd-grpcd`（Go）: LCP の requester/provider ロジック + compute backend + LCPD gRPC API

プロセス間は “lnd 互換” ではなく “LCP 向けの専用 gRPC” で接続します。`go-lcpd` は lnd の `.proto` や lnrpc/routerrpc 型を参照しない（参照を残す場合でも、lnd backend のみに閉じ込める）ようにします。

責務の境界は次のとおりです。

- `apps/ldk-lcp-node` は Lightning の “ノード機能” を担当します（peer 接続、チャネル、支払い、invoice、custom message の送受信）。
- `go-lcpd` は LCP の “アプリケーション層” を担当します（LCP wire の encode/decode、manifest 管理、quote/pay/stream/result の状態遷移、compute backend）。

MVP のスコープは「LCP を動かすことに特化した最小」に寄せます。

- In scope（MVP）: direct channel 前提の支払い、協調 close、LCP の custom message の raw 送受信、description_hash invoice（terms_hash による束縛）、Esplora chain sync、Rapid Gossip Sync
- Out of scope（当面）: マルチホップ支払い、オートパイロット、watchtower、オンチェーン送金 UI/高度なウォレット UX、複雑な fee policy

### Milestone 1: Node API（proto）を定義し、go-lcpd 側の抽象を導入する

`go-lcpd` と LDK ノードの間で必要な操作を定義する `.proto` を追加し、Go の client 実装を作れる状態にします。

`.proto` の配置は、既存の proto-go 生成フローに合わせて `go-lcpd/proto/` 配下に置きます。

追加するファイル（提案）:

- `go-lcpd/proto/lnnode/v1/lnnode.proto`

MVP で定義する RPC（提案）:

    service LightningNodeService {
      rpc GetNodeInfo(GetNodeInfoRequest) returns (GetNodeInfoResponse);

      // Peer + custom messages (LCP transport)
      rpc ConnectPeer(ConnectPeerRequest) returns (ConnectPeerResponse);
      rpc ListPeers(ListPeersRequest) returns (ListPeersResponse);
      rpc SubscribePeerEvents(SubscribePeerEventsRequest) returns (stream PeerEvent);

      rpc SendCustomMessage(SendCustomMessageRequest) returns (SendCustomMessageResponse);
      rpc SubscribeCustomMessages(SubscribeCustomMessagesRequest) returns (stream CustomMessage);

      // Channels (operator needs this for independence)
      rpc OpenChannel(OpenChannelRequest) returns (OpenChannelResponse);
      rpc CloseChannel(CloseChannelRequest) returns (CloseChannelResponse);
      rpc ListChannels(ListChannelsRequest) returns (ListChannelsResponse);

      // Wallet (minimum to fund channels)
      rpc NewAddress(NewAddressRequest) returns (NewAddressResponse);
      rpc WalletBalance(WalletBalanceRequest) returns (WalletBalanceResponse);
      rpc SendToAddress(SendToAddressRequest) returns (SendToAddressResponse);

      // Invoices + payments (for go-lcpd)
      rpc CreateInvoice(CreateInvoiceRequest) returns (CreateInvoiceResponse);
      rpc WaitInvoiceSettled(WaitInvoiceSettledRequest) returns (WaitInvoiceSettledResponse);
      rpc DecodeInvoice(DecodeInvoiceRequest) returns (DecodeInvoiceResponse);
      rpc PayInvoice(PayInvoiceRequest) returns (PayInvoiceResponse);
    }

設計上の注意:

- pubkey や hash は proto の `bytes` で表現する（hex string ではなく）。Go 側で `hex.EncodeToString` が必要なら adapter 内で行う。
- `CreateInvoice` は “description_hash を必ずセットできる” ことが必須。LCP は `terms_hash` を invoice `description_hash` に束縛するため。
- `WaitInvoiceSettled` は streaming ではなく unary にしても良い（MVP は “特定 invoice の settled を待つ” だけで十分）。`go-lcpd` 側の `InvoiceCreator` もこれに合わせて変更して良い。

MVP の message shape（提案、proto3 の擬似定義）:

    // NOTE: pubkey は compressed secp256k1（33 bytes）を想定する。
    // NOTE: hash は 32 bytes を想定する。

    message GetNodeInfoRequest {}
    message GetNodeInfoResponse {
      bytes node_pubkey = 1;
      string network = 2; // "regtest" | "testnet" | "mainnet"
      repeated string p2p_listen_addrs = 3; // "host:port"
    }

    message ConnectPeerRequest { bytes peer_pubkey = 1; string addr = 2; } // "host:port"
    message ConnectPeerResponse { bool already_connected = 1; }

    message ListPeersRequest {}
    message Peer { bytes peer_pubkey = 1; string addr = 2; bool connected = 3; }
    message ListPeersResponse { repeated Peer peers = 1; }

    message SubscribePeerEventsRequest {}
    message PeerEvent {
      bytes peer_pubkey = 1;
      enum Type { TYPE_UNSPECIFIED = 0; TYPE_ONLINE = 1; TYPE_OFFLINE = 2; }
      Type type = 2;
    }

    message SendCustomMessageRequest { bytes peer_pubkey = 1; uint32 msg_type = 2; bytes data = 3; }
    message SendCustomMessageResponse {}

    message SubscribeCustomMessagesRequest {
      // 空なら全て。指定されていればその msg_type のみ受け取る。
      repeated uint32 msg_types = 1;
    }
    message CustomMessage { bytes peer_pubkey = 1; uint32 msg_type = 2; bytes data = 3; }

    message OpenChannelRequest {
      bytes peer_pubkey = 1;
      uint64 local_funding_amount_sat = 2;
      bool announce_channel = 3;
    }
    message OpenChannelResponse {
      bytes channel_id = 1;   // 32 bytes
      string funding_txid_hex = 2; // block explorer に貼る txid hex と同じ表現
    }

    message CloseChannelRequest { bytes channel_id = 1; bool force = 2; }
    message CloseChannelResponse {}

    message ListChannelsRequest {}
    message Channel {
      bytes channel_id = 1;
      bytes peer_pubkey = 2;
      uint64 channel_value_sat = 3;
      uint64 outbound_capacity_msat = 4;
      uint64 inbound_capacity_msat = 5;
      bool usable = 6;
    }
    message ListChannelsResponse { repeated Channel channels = 1; }

    message NewAddressRequest {}
    message NewAddressResponse { string address = 1; }

    message WalletBalanceRequest {}
    message WalletBalanceResponse { uint64 confirmed_sat = 1; uint64 unconfirmed_sat = 2; }

    message SendToAddressRequest {
      string address = 1;
      uint64 amount_sat = 2;
      uint64 fee_rate_sat_per_vbyte = 3;
      bool rbf = 4;
      bytes idempotency_key = 5; // optional but strongly recommended for retries
    }
    message SendToAddressResponse { string txid_hex = 1; }

    message CreateInvoiceRequest {
      bytes description_hash = 1; // 32 bytes
      uint64 amount_msat = 2;
      uint64 expiry_seconds = 3;
    }
    message CreateInvoiceResponse { string payment_request = 1; bytes payment_hash = 2; }

    message WaitInvoiceSettledRequest { bytes payment_hash = 1; uint32 timeout_seconds = 2; }
    message WaitInvoiceSettledResponse {
      enum State { STATE_UNSPECIFIED = 0; STATE_SETTLED = 1; STATE_CANCELED = 2; STATE_EXPIRED = 3; }
      State state = 1;
    }

    message DecodeInvoiceRequest { string payment_request = 1; }
    message DecodeInvoiceResponse {
      bytes payee_pubkey = 1;
      bytes description_hash = 2;
      uint64 amount_msat = 3;
      uint64 timestamp_unix = 4;
      uint64 expiry_seconds = 5;
    }

    message PayInvoiceRequest {
      string payment_request = 1;
      uint32 timeout_seconds = 2;
      uint64 fee_limit_msat = 3;
    }
    message PayInvoiceResponse {
      enum Status { STATUS_UNSPECIFIED = 0; STATUS_SUCCEEDED = 1; STATUS_FAILED = 2; }
      Status status = 1;
      bytes payment_preimage = 2; // success only
      string failure_message = 3; // failed only (human-readable)
    }

go-lcpd 側の変更（設計）:

- `go-lcpd/internal/lightningnode`（新規）に “ノード backend” の interface と実装を集約する。
  - `internal/lightningnode/lndgrpc`（既存の lnd 実装を移植/ラップ）
  - `internal/lightningnode/ldkgrpc`（新規、LDK ノードの gRPC client）
- `go-lcpd/internal/lndpeermsg` は LND 固有なので、LCP 側に寄せた名前（例: `go-lcpd/internal/peermsg`）に置き換える。
  - 新 `peermsg` は backend interface だけに依存し、`PeerDirectory` 更新や `lcp_manifest` 送信等の既存ロジックを維持する。
- `go-lcpd/tools/lcpd-grpcd` の設定を “lnd 前提” から “backend 選択” に変更する（例: `LCPD_LIGHTNING_BACKEND=ldkgrpc|lndgrpc|disabled` と `LCPD_LN_NODE_GRPC_ADDR=...`）。
- 既存の lnd backend は当面残してよい（移行の安全性）。ただし LCP のコアロジックは lnd 型に依存しない。

Milestone 1 の受け入れ:

- `cd go-lcpd && buf generate --template ../proto-go/buf.gen.yaml` が通り、`proto-go/` に `lnnode/v1` の Go 型が生成される。
- `go-lcpd` に “Lightning ノード backend を差し替えられる” 構造が入っている（ldk 実装はスタブでも良い）。

### Milestone 2: LDK ノード daemon（MVP）を実装する

新規 Rust プロジェクトとして `apps/ldk-lcp-node/` を追加します。ここが “独立ノード” の本体です。

MVP の責務:

- peer 管理: `pubkey@host:port` で接続でき、接続イベントが取れる
- チャネル: open/close/list ができる（協調 close をまず優先）
- 支払い: BOLT11 を decode でき、direct channel 前提で pay できる
- 請求: description_hash を指定して invoice を作成でき、settled を検知できる
- custom message: 任意の `msg_type(u16)` と `payload(bytes)` を送信でき、受信を stream で配信できる

LDK 構成（提案）:

- `PeerManager` + `lightning-net-tokio`（P2P）
- `ChannelManager` + `ChainMonitor` + `lightning-background-processor`（チャネル/イベント処理）
- on-chain 同期: Esplora を chain source として使う（`--esplora-base-url` で指定）
- 鍵/状態永続化: `data_dir/` に保存し、再起動で復元する

この milestone のゴールは「ノード単体で peer/チャネル/支払いが成立する」ことです。まだ `go-lcpd` 連携は必須ではありません。

Milestone 2 の受け入れ:

- regtest 上で 2 台起動し、`ConnectPeer` と `OpenChannel` が成立する。
- `ListChannels` が期待する情報を返す。

### Milestone 3: custom message を go-lcpd に橋渡しし、LCP transport を成立させる

`go-lcpd` は inbound custom message を受け取って `peerDirectory` を更新し、`lcp_manifest` を送る必要があります。現状これは `internal/lndpeermsg` が担当しています。

ここでは `internal/lndpeermsg` を置き換える実装（例: `internal/lnnodepeermsg`）を作り、LDK ノードの `SubscribePeerEvents` / `SubscribeCustomMessages` を購読して、既存の inbound dispatch に流します。

Milestone 3 の受け入れ:

- 2 台の `lcpd-grpcd` が LDK ノード経由で `lcp_manifest` を交換し、`ListLCPPeers` が LCP-ready peer を列挙できる。

### Milestone 4: Provider/Requester の支払い・請求を LDK ノードで成立させ、LCP E2E を完走する

Provider 側:

- `CreateInvoice(description_hash=terms_hash, amount_msat=price_msat, expiry=...)` ができる。
- `WaitInvoiceSettled(payment_hash)` で settle を待てる。

Requester 側:

- `DecodeInvoice` が `description_hash`/`payee_pubkey`/`amount_msat` を返し、go-lcpd の既存検証（terms_hash など）に使える。
- `PayInvoice` が preimage を返せる。

Milestone 4 の受け入れ:

- regtest 上で LCP の end-to-end（quote → pay → stream(result) → result）が完走する。

### Milestone 5: 起動高速化と安定化（再起動耐性）

MVP 完了後、安定運用に必要な要素を入れます。

- 永続化の整備（チャネル状態、monitor、支払い追跡、peer アドレス帳）
- 再起動時の復元（未確定の支払い/受け取りの扱い）
- ログとメトリクス（秘密情報やペイロードを不用意にログに出さない）

## Concrete Steps

実装に入ったら、このセクションを milestone ごとに更新すること。

例（Milestone 1: proto 生成）:

    cd go-lcpd
    buf generate --template ../proto-go/buf.gen.yaml

例（Milestone 2: ノード起動）:

    cd apps/ldk-lcp-node
    cargo run -- \
      --data-dir ./dev-data \
      --grpc-addr 127.0.0.1:10010 \
      --p2p-listen 0.0.0.0:9736 \
      --network regtest \
      --esplora-base-url http://127.0.0.1:3000 \
      --rgs-base-url http://127.0.0.1:3000

## Validation and Acceptance

この plan の完了条件（最終受け入れ）は次のとおりです。

- `go-lcpd` の unit tests が通る（`cd go-lcpd && go test ./...`）。
- regtest 上で、LDK ノード 2 台 + `lcpd-grpcd` 2 台で LCP E2E が完走する。
- ノード再起動後もチャネル状態と最低限の運用（peer 接続、custom message、支払い/請求）が復元される。

## Idempotence and Recovery

- `apps/ldk-lcp-node` の `data_dir` は再利用可能であること（誤って削除するとチャネル state を失うため、本番では削除しない運用にする）。
- 開発用 regtest の失敗は「dev-data を退避して再初期化」できるようにする（ただしチャネルが消える）。

## Artifacts and Notes

（実装時に追記）:

- 主要ログ断片
- `lcpdctl` での確認コマンド例
- regtest の手順書（最短手順）

## Interfaces and Dependencies

### `go-lcpd` が Lightning ノードに要求する最小 interface（MVP）

`go-lcpd` 内部で必要な能力は、LCP の要件に絞ると次です。

- local node pubkey を取得できる（`GetNodeInfo` 相当）
- custom message の送信/受信ができる
- invoice の decode / pay ができる
- invoice の create / settle 待ちができる

チャネル管理は “独立ノード” としては必須ですが、LCP ロジック自体が常に呼ぶとは限りません（運用 CLI で行っても良い）。そのため API は提供しつつ、go-lcpd のコアからの依存は最小化します。

### Rust dependencies（提案）

- async/runtime: `tokio`
- gRPC: `tonic`
- LDK core: `lightning`, `lightning-background-processor`, `lightning-net-tokio`, `lightning-invoice`, `lightning-custom-message`
- chain sync（MVP）: `esplora-client`
- gossip sync（MVP）: `lightning-rapid-gossip-sync`

---

Plan changes:

- (2026-01-06) ユーザー要望により、lnd shim 案から「独立 LDK ノード + go-lcpd 変更許容」へ方針転換し、MVP の責務にチャネル open/close を含めた。
