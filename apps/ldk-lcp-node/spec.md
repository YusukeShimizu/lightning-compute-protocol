# spec.md — ldk-lcp-node (LDK Lightning Node for LCP)

This document is the implementation specification (single source of truth) for `apps/ldk-lcp-node/` in this repository.
It is written in WYSIWID format (Concepts + Synchronizations).

`ldk-lcp-node` is a standalone Lightning node daemon built on LDK (Rust) that is optimized for running LCP via `go-lcpd/`.
It is not a general-purpose replacement for `lnd` in the short term; it is an “LCP-first” node that focuses on:

- stable peer connectivity (direct peering required by LCP)
- BOLT #1 custom messages (raw send/receive; LCP lives above this)
- channel open/close (operator UX)
- BOLT11 invoice creation + settlement detection (provider side)
- BOLT11 invoice decoding + payment (requester side)

Chain sync uses Esplora. Gossip sync uses Rapid Gossip Sync (RGS).
Payments are restricted to **direct channels only** (no multi-hop) in the MVP.

Primary external interfaces:

- Node gRPC API: `lnnode.v1.LightningNodeService` (new `.proto`, to be added under `go-lcpd/proto/lnnode/v1/`)
- go-lcpd integration: `go-lcpd` will call the Node gRPC API via a dedicated backend client (e.g. `go-lcpd/internal/lightningnode/ldkgrpc`)

## Security & Architectural Constraints

These constraints are system invariants.

- Process boundary: `ldk-lcp-node` MUST be a separate process from `go-lcpd`. Rationale: isolate concerns, simplify builds, and avoid Go↔Rust in-process FFI complexity.

- Transport neutrality: `ldk-lcp-node` MUST treat BOLT #1 custom messages as **raw bytes**. It MUST NOT parse, validate, or interpret LCP payloads. Rationale: keep the Lightning node and the application protocol layered; keep node reusable for future protocols.

- Custom message capability: `ldk-lcp-node` MUST support sending and receiving arbitrary custom messages identified by `msg_type(u16)` and `data(bytes)`. Rationale: LCP transport is BOLT #1 custom messages.

- Direct-channel-only payments (MVP): `ldk-lcp-node` MUST NOT attempt multi-hop payments. If a payment cannot be made over a single direct channel to the invoice payee, it MUST fail with an explicit error. Rationale: reduce routing complexity and eliminate dependency on a full gossip graph for MVP.

- Esplora chain sync: `ldk-lcp-node` MUST use Esplora (HTTP) as its chain backend. Rationale: fastest operator experience; avoids maintaining a local full node for typical deployments.

- Rapid Gossip Sync: `ldk-lcp-node` MUST support Rapid Gossip Sync (RGS) to keep a Network Graph fresh. Rationale: fast startup and future-proofing (even if MVP payments do not use it).

- Persistence: `ldk-lcp-node` MUST persist all state required for safe restart: channel manager state, channel monitors, wallet state, peer store, and in-flight invoice/payment tracking. It MUST be crash-safe (atomic writes) and MUST not corrupt state on power loss. Rationale: stability and restart safety.

- Secret handling: `ldk-lcp-node` MUST NOT log secrets (seed material, xpriv, payment preimages, invoice strings, macaroon-like tokens, custom message payloads). Rationale: logs are frequently exported/aggregated.

- RPC exposure: The gRPC server MUST bind to loopback by default. If configured to bind to non-loopback addresses, the node MUST require authentication (e.g., a static bearer token) and SHOULD require TLS. Rationale: node RPC can move funds.

- Deterministic identifiers: When returning identifiers over the RPC (channel IDs, txids, hashes), the node MUST use a single canonical byte order and document it. Rationale: avoid common byte-order bugs across languages.

## Concepts

Concepts are independent capabilities. They MUST NOT depend on each other.

### Common Data Shapes (normative)

These shapes are used across the document and across the gRPC boundary.

- `NodePubKey33`: 33 bytes, compressed secp256k1 public key.
- `Hash32`: 32 bytes.
- `ChannelId32`: 32 bytes (LDK channel ID).
- `CustomMsgTypeU16`: unsigned 16-bit integer.
- `Msat`: unsigned 64-bit integer (milli-satoshis).
- `Sat`: unsigned 64-bit integer (satoshis).

Byte order conventions:

- For `NodePubKey33`, `Hash32`, and `ChannelId32`, the byte order is the natural byte order of the underlying value.
- For Bitcoin txids and block hashes, the gRPC API MUST use the conventional human hex string representation (the same hex you would paste into block explorers). This avoids cross-language “reversed hash” confusion.

### NodeConfig

Purpose: Define all operator-configurable settings and provide a validated runtime configuration.

Domain Model:

- `Network`: `"mainnet" | "testnet" | "signet" | "regtest"`
- `DataDir`: filesystem path
- `EsploraBaseURL`: string (HTTP base URL)
- `RGSBaseURL`: string (HTTP base URL)
- `GRPCListenAddr`: string (e.g., `127.0.0.1:10010`)
- `P2PListenAddrs`: list of `host:port`
- `LogLevel`: string
- `DirectPaymentsOnly`: bool (MVP: always true)
- `PaymentFeeLimitMsat`: u64 (default bounded)
- `PaymentTimeoutSeconds`: u32 (default bounded)
- `RPCAuthToken`: optional string

Actions:

- `load_config(args, env) -> config | error`
- `validate_config(config) -> config | error`

Operational Principle:

- If `GRPCListenAddr` is non-loopback, `RPCAuthToken` MUST be set (and TLS SHOULD be enabled).
- `EsploraBaseURL` MUST be provided explicitly; there is no “magic default” baked into the binary.

### DurableStore

Purpose: Provide crash-safe persistence of node state under `DataDir`.

Domain Model:

- `StoreKey`: string
- `StoreValue`: bytes
- `StoreVersion`: u32

Actions:

- `read(key) -> value | not_found | error`
- `write_atomic(key, value) -> ok | error`
- `list(prefix) -> keys | error`

Operational Principle:

- Writes MUST be atomic (write temp + fsync + rename).
- Values MUST be versioned to allow upgrades.

### KeysAndWallet

Purpose: Hold node identity keys and an on-chain wallet capable of funding channels and generating receive addresses.

Domain Model:

- `NodePubKey33`: 33-byte compressed secp256k1 pubkey
- `SeedBytes`: bytes (stored encrypted-at-rest if a password feature exists; MVP may omit password support but MUST still avoid leaking it)
- `Address`: string
- `Balance`: confirmed_sat, unconfirmed_sat
- `OnchainTxidHex`: string (standard human txid hex)
- `FeeRateSatPerVByte`: u64

Actions:

- `init_or_load_keys(data_dir) -> node_pubkey | error`
- `new_address() -> address | error`
- `wallet_balance() -> balance | error`
- `send_to_address(address, amount_sat, fee_rate_sat_per_vbyte, rbf, idempotency_key) -> txid_hex | error`
- `funding_tx_create(amount_sat, target_script) -> tx | error`
- `funding_tx_sign(tx) -> signed_tx | error`

Operational Principle:

- The node MUST keep a stable `NodePubKey33` across restarts (unless `DataDir` is deleted).
- `send_to_address` MUST validate that the address matches the configured Bitcoin network (e.g., mainnet vs testnet/signet/regtest).
- `send_to_address` SHOULD default to RBF-enabled transactions to allow fee bumping, but MUST make the behavior explicit in the API.
- `send_to_address` MUST be idempotent when `idempotency_key` is provided: repeated calls with the same key MUST return the same `txid_hex` (or a stable error if the original attempt failed before broadcast). Rationale: gRPC clients may retry on timeouts and must not double-spend by accident.

### EsploraChainSync

Purpose: Keep the node’s channel state and wallet state synchronized with the Bitcoin chain using Esplora.

Domain Model:

- `EsploraClient`: configured by `EsploraBaseURL`
- `ChainTip`: height, block_hash
- `SyncState`: last_synced_tip, last_synced_at

Actions:

- `sync_once() -> ok | error`
- `sync_loop(interval) -> never` (runs until shutdown)

Operational Principle:

- Sync MUST be incremental and safe to retry.
- Sync MUST drive both channel-related watch data and wallet-related watch data.

### RapidGossipSync

Purpose: Keep a Lightning network graph current using Rapid Gossip Sync snapshots.

Domain Model:

- `RGSClient`: configured by `RGSBaseURL`
- `GraphSyncState`: last_sync_timestamp, last_sync_at

Actions:

- `sync_graph_once() -> ok | error`
- `sync_graph_loop(interval) -> never`

Operational Principle:

- RGS sync MUST be optional at runtime (config toggle), but supported by design.
- MVP payments MUST NOT depend on the graph being present.

### PeerConnectivity

Purpose: Maintain peer connections and expose peer status/events.

Domain Model:

- `PeerPubKey33`
- `PeerAddr`: string (`host:port`)
- `PeerStatus`: connected | disconnected

Actions:

- `connect_peer(peer_pubkey, addr) -> ok | already_connected | error`
- `list_peers() -> []peer`
- `subscribe_peer_events() -> stream(peer_event)`

Operational Principle:

- Peer connection attempts MUST be idempotent.
- The node SHOULD keep outbound connections alive (reconnect with backoff).

### ChannelOperations

Purpose: Allow an operator (or automation) to open/close/list channels.

Domain Model:

- `ChannelId32`
- `FundingTxidHex`: string (standard human txid hex)
- `Channel`: channel_id, peer_pubkey, value_sat, usable(bool), outbound_msat, inbound_msat

Actions:

- `open_channel(peer_pubkey, amount_sat, announce) -> (channel_id, funding_txid_hex) | error`
- `close_channel(channel_id, force) -> ok | error`
- `list_channels() -> []channel`

Operational Principle:

- Open/close MUST be persisted as a state transition before returning success.
- For MVP, cooperative close MUST be implemented; force close MAY be added later.

### CustomMessageTransport

Purpose: Provide raw BOLT #1 custom message send/receive to local clients (e.g., go-lcpd).

Domain Model:

- `CustomMsgTypeU16`
- `CustomMessage`: peer_pubkey, msg_type, data

Actions:

- `send_custom_message(peer_pubkey, msg_type, data) -> ok | error`
- `subscribe_custom_messages(filter_msg_types[]) -> stream(custom_message)`

Operational Principle:

- Backpressure MUST exist (bounded queues) to avoid unbounded memory growth.
- Dropping messages MUST be observable (metrics/log counters), but MUST NOT crash the node.

### InvoiceLifecycle

Purpose: Create BOLT11 invoices with `description_hash` and report settlement.

Domain Model:

- `Hash32` (payment_hash, description_hash)
- `InvoiceRequest`: description_hash, amount_msat, expiry_seconds
- `CreatedInvoice`: payment_request(string), payment_hash(Hash32)
- `InvoiceState`: settled | canceled | expired | unknown

Actions:

- `create_invoice(req) -> created_invoice | error`
- `wait_invoice_settled(payment_hash, timeout) -> invoice_state | error`

Operational Principle:

- The created invoice MUST commit to `description_hash` exactly as provided.
- The node MUST automatically claim inbound funds for invoices it creates, so that settlement is reached without manual intervention.

### PaymentLifecycle

Purpose: Decode BOLT11 invoices and pay them over a direct channel.

Domain Model:

- `DecodedInvoice`: payee_pubkey, description_hash, amount_msat, timestamp_unix, expiry_seconds
- `PaymentOutcome`: succeeded(preimage) | failed(message)

Actions:

- `decode_invoice(payment_request) -> decoded_invoice | error`
- `pay_invoice(payment_request, timeout, fee_limit_msat) -> outcome | error`

Operational Principle:

- If `DirectPaymentsOnly` is true, `pay_invoice` MUST only use a single-hop route over a direct channel to `payee_pubkey`.
- On success, the node MUST return the 32-byte payment preimage.

### LightningNodeRPC

Purpose: Expose the node capabilities over gRPC for `go-lcpd` and operator tooling.

Domain Model:

- `lnnode.v1.LightningNodeService` RPC methods (normative API surface)

Actions:

- `serve_grpc(listen_addr) -> never`

Operational Principle:

- RPC methods MUST map 1:1 to the node capabilities described in this document, without embedding LCP-specific semantics.
- Errors MUST be stable and actionable (e.g., `FAILED_PRECONDITION: no_direct_channel_to_payee`).

Normative RPC surface (method list):

- `GetNodeInfo() -> node_pubkey, network, p2p_listen_addrs, chain_sync_state, gossip_sync_state`
- `ConnectPeer(peer_pubkey, addr) -> already_connected`
- `ListPeers() -> peers[]`
- `SubscribePeerEvents() -> stream(peer_event)`

- `SendCustomMessage(peer_pubkey, msg_type, data) -> ok`
- `SubscribeCustomMessages(msg_types[]) -> stream(custom_message)`

- `OpenChannel(peer_pubkey, local_funding_amount_sat, announce_channel) -> channel_id, funding_txid_hex`
- `CloseChannel(channel_id, force) -> ok`
- `ListChannels() -> channels[]`

- `NewAddress() -> address`
- `WalletBalance() -> confirmed_sat, unconfirmed_sat`
- `SendToAddress(address, amount_sat, fee_rate_sat_per_vbyte, rbf, idempotency_key) -> txid_hex`

- `CreateInvoice(description_hash, amount_msat, expiry_seconds) -> payment_request, payment_hash`
- `WaitInvoiceSettled(payment_hash, timeout_seconds) -> state`
- `DecodeInvoice(payment_request) -> payee_pubkey, description_hash, amount_msat, timestamp_unix, expiry_seconds`
- `PayInvoice(payment_request, timeout_seconds, fee_limit_msat) -> status, payment_preimage, failure_message`

Normative RPC semantics (selected):

- `SendCustomMessage` MUST reject `msg_type < 32768` with `INVALID_ARGUMENT`. Rationale: only BOLT #1 custom message range is supported.
- `SubscribeCustomMessages`: if `msg_types` is empty, all custom messages are delivered; otherwise only messages matching one of the types are delivered.
- `PayInvoice`: if there is no usable direct channel to the decoded payee pubkey, return `FAILED_PRECONDITION` with a stable message (e.g., `no_direct_channel_to_payee`).
- `CreateInvoice`: MUST set the invoice description hash to the provided 32 bytes exactly.
- `WaitInvoiceSettled`: MUST return `SETTLED` only after the node has claimed the funds and considers the inbound payment finalized.
- `SendToAddress`: MUST broadcast the transaction via the configured chain backend and return the resulting `txid_hex` on success.
- `SendToAddress`: MUST enforce conservative bounds (max fee rate, max absolute fee) to prevent accidental wallet drain. The bounds MUST be configurable.

## Synchronizations

Synchronizations connect Concepts into end-to-end flows.

### sync NodeStartup

Summary: start the daemon, restore state, and become ready for gRPC requests.

Flow:

- When the process starts, load and validate config.
- Where `DataDir` exists, load persisted state; otherwise initialize new state and persist it.
- Then start P2P listeners and the gRPC server.
- Then begin the background loops for Esplora chain sync and RGS graph sync.

### sync OperatorConnectPeer

Summary: ensure a target peer is connected.

Flow:

- When `ConnectPeer(peer_pubkey, addr)` is called, attempt to connect or return `already_connected`.
- Then emit a peer online event once the connection is established.

### sync OperatorDeposit (on-chain)

Summary: receive on-chain funds into the node wallet.

Flow:

- When an operator calls `NewAddress`, the node returns a new on-chain address.
- Then the operator sends bitcoin to that address externally (exchange, faucet, another wallet).
- Then Esplora chain sync observes the new UTXO and updates the wallet balance.

### sync OperatorWithdraw (on-chain)

Summary: send on-chain funds out of the node wallet.

Flow:

- When an operator calls `SendToAddress(address, amount_sat, ...)`, the node constructs, signs, and broadcasts a transaction.
- Then Esplora chain sync observes the transaction and the wallet balance reflects it.
- If the operator needs to withdraw funds that are currently locked in channels, they must close those channels first (`CloseChannel`) and wait for the close to confirm and outputs to be swept into the wallet.

### sync OperatorOpenChannel

Summary: open a channel to a peer using wallet funds.

Flow:

- When `OpenChannel(peer_pubkey, amount_sat)` is called, verify the wallet has sufficient funds.
- Then initiate channel open and construct/broadcast the funding transaction when required.
- Then persist state and return `channel_id` and `funding_txid_hex`.

### sync OperatorCloseChannel

Summary: close a channel (cooperative first).

Flow:

- When `CloseChannel(channel_id, force=false)` is called, request cooperative close.
- Then monitor until the close is confirmed by chain sync.

### sync LCPTransport (go-lcpd)

Summary: use the node only as a transport + payment backend for LCP.

Flow:

- When `go-lcpd` starts with `LCPD_LIGHTNING_BACKEND=ldkgrpc`, it connects to `LightningNodeService`.
- Where it needs to send LCP wire messages, it calls `SendCustomMessage(peer_pubkey, msg_type, payload)`.
- Then it consumes inbound messages via `SubscribeCustomMessages` and routes them through existing LCP logic (manifest gating, replay checks, etc.).

### sync ProviderInvoiceAndSettlement (go-lcpd)

Summary: provider creates an invoice bound to `terms_hash`, then waits for settlement before executing.

Flow:

- When `go-lcpd` decides on `terms_hash` and `price_msat`, it calls `CreateInvoice(description_hash=terms_hash, amount_msat=price_msat, expiry=...)`.
- Then it sends the invoice string to the requester via LCP custom messages.
- Then it calls `WaitInvoiceSettled(payment_hash)` and blocks until settled.

### sync RequesterDecodeAndPay (go-lcpd)

Summary: requester validates and pays a provider invoice.

Flow:

- When `go-lcpd` receives an invoice string, it calls `DecodeInvoice` to obtain `description_hash`, `payee_pubkey`, and `amount_msat`.
- Then it validates these against the LCP `terms_hash` and peer identity.
- Then it calls `PayInvoice`. If there is no direct channel, it fails fast and surfaces an actionable error to the user.

---

Spec changes:

- (2026-01-06) Initial version. Direct-channel-only payments, Esplora chain sync, and Rapid Gossip Sync are first-class constraints.
