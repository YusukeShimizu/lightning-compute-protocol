# lcp-quickstart spec (WYSIWID) — testnet/mainnet

This document follows the “What You See Is What It Does (WYSIWID)” pattern:
Concepts describe independent capabilities; Synchronizations connect them into end-to-end user-visible flows.

## Security & Architectural Constraints

The keywords **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are to be interpreted as described in RFC 2119.

- The app MUST support only `testnet` and `mainnet` networks.
  Rationale: keep the UX focused; `regtest` is intentionally out of scope.
- The app MUST treat mainnet as dangerous by default.
  Rationale: mainnet actions can move real funds and cause irreversible loss.
- The app MUST require explicit user consent for mainnet “funds-moving” operations.
  Rationale: avoid accidental channel opens or unintended spend enablement.
- The app MUST NOT automatically create or unlock a wallet (testnet or mainnet).
  Rationale: unlocking/creation involves secret handling (password/seed) and requires a dedicated UX.
- The app MUST NOT print or persist seed mnemonics.
  Rationale: seed phrases are total-funds secrets.
- The app MUST NOT log OpenAI prompts/outputs, API keys, macaroons, or BOLT11 invoices.
  Rationale: logs are commonly shared; leaks are catastrophic.
- The app MUST bind local services (`lcpd-grpcd`, `openai-serve`, optional local API for UI) to `127.0.0.1` by default.
  Rationale: these processes can spend funds or expose sensitive metadata if reachable remotely.
- The app MUST refuse to bind `lcpd-grpcd` to non-loopback addresses.
  Rationale: `lcpd-grpcd` has no built-in auth and can spend sats via the user’s `lnd`.
- The app MUST refuse to bind `openai-serve` to non-loopback addresses unless the user passes `--i-understand-exposing-openai` and configures API keys (`--openai-api-keys` non-empty).
  Rationale: exposing the OpenAI-compatible HTTP surface without explicit consent/auth is unsafe.
- The app MAY auto-install `lnd` and `lncli` by downloading official release binaries into the workspace `bin/` directory.
  Rationale: reduce onboarding friction and avoid OS-specific package managers.
- If auto-installing `lnd`/`lncli`, the app SHOULD verify the tarball SHA256 using the release `manifest-<version>.txt`.
  Rationale: detect corruption and avoid partially downloaded/cached artifacts.
- The app MUST pin `openai-serve` routing to a single Provider peer id (`OPENAI_SERVE_DEFAULT_PEER_ID`) for both testnet and mainnet.
  Rationale: prevent accidental routing to an arbitrary connected peer.
- The app SHOULD set a conservative `OPENAI_SERVE_MAX_PRICE_MSAT` default for mainnet.
  Rationale: reduce overspend risk from misconfiguration or unexpected quotes.
- The app MUST validate that `lncli` is targeting the requested network (`testnet` or `mainnet`).
  Rationale: mixing networks is a common source of confusing failures and accidental mainnet use.
- The app MUST be restartable and idempotent for the “up” flows.
  Rationale: users will rerun commands during setup; partial progress must not require manual cleanup.
- The app MUST support a destructive reset that requires `--force`.
  Rationale: avoid accidental deletion of app state/logs (and any managed lnd directories).

## Concepts

### QuickstartWorkspace

- Purpose: Provide a stable on-disk “home” for state/logs/pids that both CLI and future UI can consume.
- Domain Model:
  - `Workspace: data_dir, state_path, logs_dir, pids_dir`
- Actions:
  - `ResolveWorkspace(home_override: string) -> Workspace`
    - Side effects: MAY create directories.
    - Errors: none (falls back to `~/.lcp-quickstart` on failure to resolve override).
  - `LogPath(workspace: Workspace, component: string) -> string`
  - `PIDPath(workspace: Workspace, component: string) -> string`

### QuickstartStateStore

- Purpose: Persist a machine-readable snapshot of “what’s running” and “what’s ready” for CLI status and future UI.
- Domain Model:
  - `Network: "testnet" | "mainnet"`
  - `ComponentState: running: bool, pid: int, addr: string, last_error: string`
  - `Readiness: lnd_ready: bool, provider_connected: bool, channel_ready: bool, lcpd_ready: bool, openai_ready: bool`
  - `State: version, network, provider_node, provider_peer_id, readiness, components: map[string]ComponentState, updated_at_rfc3339`
- Actions:
  - `Read(path: string) -> State | (empty State when file missing)`
    - Errors: returns an error only on unreadable/invalid JSON.
  - `Write(path: string, state: State) -> ok | error`
    - Side effects: atomic write (write temp + rename).
  - `Update(path: string, mutate: func(State) State) -> State | error`
    - Side effects: MUST be serialized (file lock or best-effort lock) to avoid concurrent writes.

### LNCLI

- Purpose: Interact with the user’s lnd via `lncli` for the target network (testnet/mainnet).
- Domain Model:
  - `Network: "testnet" | "mainnet"`
  - `ProviderNode: "<pubkey>@<host:port>"`
  - `ProviderPeerID: "<pubkey_hex_66>"`
  - `WalletState: "uninitialized" | "locked" | "unlocked" | "unknown"`
- Actions:
  - `GetInfo(network: Network) -> stdout_json_bytes | error`
    - Side effects: runs `lncli` (for testnet, MUST select testnet).
  - `ValidateNetwork(network: Network, getinfo_json: bytes) -> ok | error`
    - Errors: returns error when `getinfo.chains[].network` is not the requested network.
  - `DetectWalletState(network: Network) -> WalletState`
    - Behavior: best-effort classification based on `lncli getinfo` success/failure output.
    - Side effects: MUST NOT print mnemonics (this action MUST NOT call `lncli create`).
  - `ConnectPeer(network: Network, provider: ProviderNode) -> ok | error`
  - `HasOutboundLiquidity(network: Network, provider_peer_id: ProviderPeerID, min_sats: int64) -> bool | error`
    - Side effects: runs `lncli listchannels` / `lncli channelbalance` and inspects results.
  - `OpenChannel(network: Network, provider_peer_id: ProviderPeerID, local_amt_sats: int64) -> ok | error`
    - Side effects: runs `lncli openchannel ...`.
    - Errors: policy rejection, insufficient on-chain funds, fees, timeouts.

### ManagedLND

- Purpose: Optionally start `lnd` as a managed child process (without creating/unlocking wallets).
- Domain Model:
  - `LNDRun: network, lnddir, extra_args`
- Actions:
  - `Start(run: LNDRun, log_path: string, pid_path: string) -> pid | error`
    - Side effects: starts `lnd`, writes pid file, redirects stdout/stderr to log.
  - `Stop(pid_path: string) -> ok | error`
  - `IsRunning(pid_path: string) -> bool`

### LCPDGRPCD

- Purpose: Build and run `lcpd-grpcd` as a background process (Requester mode).
- Domain Model:
  - `LCPDRun: grpc_addr, lnd_rpc_addr, lnd_tls_cert_path, lnd_admin_macaroon_path`
- Actions:
  - `Build(repo_root: string, out_bin_path: string) -> ok | error`
    - Side effects: `go build` or `go install` for `go-lcpd/tools/lcpd-grpcd`.
  - `Start(workdir: string, run: LCPDRun, log_path: string, pid_path: string) -> pid | error`
    - Side effects: starts process, writes pid file, redirects stdout/stderr to log.
  - `Stop(pid_path: string) -> ok | error`
  - `IsRunning(pid_path: string) -> bool`

### OpenAIServeGateway

- Purpose: Build and run `openai-serve` to provide an OpenAI-compatible HTTP endpoint backed by LCP.
- Domain Model:
  - `OpenAIServeRun: http_addr, lcpd_grpc_addr, api_keys_csv, default_peer_id, max_price_msat`
- Actions:
  - `Build(repo_root: string, out_bin_path: string) -> ok | error`
    - Side effects: `go build` for `apps/openai-serve/cmd/openai-serve`.
  - `Start(workdir: string, run: OpenAIServeRun, log_path: string, pid_path: string) -> pid | error`
    - Side effects: starts process, writes pid file, redirects stdout/stderr to log.
  - `Stop(pid_path: string) -> ok | error`
  - `Healthz(http_addr: string) -> ok | error`
    - Side effects: HTTP GET `http://<http_addr>/healthz` with timeout.

## Synchronizations

### sync cli_testnet_up

- Summary: Connect to a testnet Provider (`--provider` required), ensure prerequisites, and start `lcpd-grpcd` + `openai-serve`.
- Flow:
  1. When: `lcp-quickstart testnet up --provider <pubkey>@<host:port> [--start-lnd] [--open-channel --channel-sats ...]`
  2. Then:
     - Resolve `workspace`.
     - If `--provider` is missing: exit non-zero with “testnet requires --provider”.
     - If `--start-lnd` is set and `LNCLI.GetInfo(testnet)` fails:
       - start `lnd` via `ManagedLND.Start(...)` and retry `LNCLI.GetInfo(testnet)`.
     - If `LNCLI.DetectWalletState(testnet)` is `uninitialized` or `locked`:
       - print the exact next command(s) to run (interactive) and exit non-zero:
         - `lcp-quickstart testnet lncli create` (first time)
         - `lcp-quickstart testnet lncli unlock` (when locked)
     - Validate `LNCLI.ValidateNetwork(testnet, getinfo_json)` succeeds.
     - Parse `provider_peer_id` from the `--provider` string.
     - `LNCLI.ConnectPeer(testnet, provider)`
     - If outbound liquidity is insufficient:
       - if `--open-channel` is not set: print “channel/route required” and exit non-zero
       - if `--channel-sats` missing: exit non-zero
       - else: `LNCLI.OpenChannel(testnet, provider_peer_id, channel_sats)` and instruct the user to wait for confirmations
     - Start `lcpd-grpcd` (Requester mode) using default `LCPD_LND_*` paths unless overridden:
       - RPC addr default: `localhost:10009`
       - TLS cert default: `~/.lnd/tls.cert`
       - Admin macaroon default: `~/.lnd/data/chain/bitcoin/testnet/admin.macaroon`
     - Start `openai-serve`:
       - `default_peer_id := provider_peer_id`
       - `api_keys_csv := "lcp-dev"`
       - `http_addr := "127.0.0.1:8080"`
       - set `max_price_msat` to `0` by default (no cap) unless overridden
     - Probe readiness:
       - `OpenAIServeGateway.Healthz(http_addr)`
     - Persist state via `QuickstartStateStore.Update(...)`.

### sync cli_testnet_down

- Summary: Stop only quickstart-managed processes (and managed `lnd` if it was started by `--start-lnd`).
- Flow:
  1. When: `lcp-quickstart testnet down`
  2. Then:
     - Stop `openai-serve` (if running)
     - Stop `lcpd-grpcd` (if running)
     - If state indicates `lnd` is managed: stop it
     - Update state to mark components stopped (keep historical info).

### sync cli_testnet_status

- Summary: Show testnet status and actionable next steps (unlock wallet, open channel, etc).
- Flow:
  1. When: `lcp-quickstart testnet status`
  2. Then:
     - Best-effort `LNCLI.GetInfo(testnet)` (show “locked/uninitialized/not running” hints on failure).
     - Read state and show managed process status.
     - If `openai-serve` configured: `Healthz`.

### sync cli_testnet_logs

- Summary: Tail logs for a chosen component.
- Flow:
  1. When: `lcp-quickstart testnet logs <component>`
  2. Then:
     - Resolve `LogPath(workspace, component)` and `tail -f` it (or print a clear error if missing).

### sync cli_testnet_reset

- Summary: Destructively remove the app workspace so `testnet up` starts fresh.
- Flow:
  1. When: `lcp-quickstart testnet reset --force`
  2. Then:
     - Run `cli_testnet_down` behavior first (best-effort).
     - Delete `workspace.data_dir`.
  3. Error branches:
     - If `--force` is missing: exit non-zero with a warning.

### sync cli_mainnet_up

- Summary: Connect to a mainnet Provider (default or `--provider`), ensure prerequisites, and start `lcpd-grpcd` + `openai-serve`.
- Flow:
  1. When: `lcp-quickstart mainnet up --i-understand-mainnet [--provider ...] [--start-lnd] [--open-channel --channel-sats ...]`
  2. Then:
     - If `--i-understand-mainnet` is missing: exit non-zero with a safety message.
     - Resolve `workspace`.
     - Ensure `LNCLI.GetInfo(mainnet)` works; if not:
       - if `--start-lnd`: start `lnd` via `ManagedLND.Start(...)` and retry
       - else: instruct the user to start/unlock `lnd`
     - If `LNCLI.DetectWalletState(mainnet)` is `uninitialized` or `locked`:
       - print “run `lcp-quickstart mainnet lncli create` / `lcp-quickstart mainnet lncli unlock`” and exit non-zero.
     - Validate `LNCLI.ValidateNetwork(mainnet, getinfo_json)` succeeds.
     - Determine Provider:
       - default: `03737b4a2e44b45f786a18e43c3cf462ab97891e9f8992a0d493394691ac0db983@54.214.32.132:20309`
       - override via `--provider`
       - parse `provider_peer_id` from the node string
     - `LNCLI.ConnectPeer(mainnet, provider)`
     - If outbound liquidity is insufficient:
       - if `--open-channel` is not set: print “channel/route required” and exit non-zero
       - if `--channel-sats` missing: exit non-zero
       - else: `LNCLI.OpenChannel(mainnet, provider_peer_id, channel_sats)` and instruct the user to wait for confirmations
     - Start `lcpd-grpcd` in Requester mode using default `LCPD_LND_*` paths unless overridden:
       - RPC addr default: `localhost:10009`
       - TLS cert default: `~/.lnd/tls.cert`
       - Admin macaroon default: `~/.lnd/data/chain/bitcoin/mainnet/admin.macaroon`
     - Start `openai-serve`:
       - set `default_peer_id := provider_peer_id`
       - set a conservative `max_price_msat` default (unless overridden)
     - Persist state.

### sync cli_mainnet_down

- Summary: Stop only quickstart-managed processes (do not stop the user’s mainnet `lnd` by default).
- Flow:
  1. When: `lcp-quickstart mainnet down`
  2. Then:
     - Stop `openai-serve` (if running)
     - Stop `lcpd-grpcd` (if running)
     - Leave `lnd` untouched unless it was started by `--start-lnd` and is tracked as managed.

### sync cli_mainnet_status

- Summary: Show mainnet status and actionable next steps (unlock wallet, open channel, etc).
- Flow:
  1. When: `lcp-quickstart mainnet status`
  2. Then:
     - Best-effort `LNCLI.GetInfo(mainnet)` (show “locked/uninitialized/not running” hints on failure).
     - Read state and show managed process status.
     - If `openai-serve` configured: `Healthz`.

### sync cli_mainnet_logs

- Summary: Tail logs for a chosen component.
- Flow:
  1. When: `lcp-quickstart mainnet logs <component>`
  2. Then:
     - Resolve `LogPath(workspace, component)` and `tail -f` it (or print a clear error if missing).

### sync cli_mainnet_reset

- Summary: Destructively remove the app workspace so `mainnet up` starts fresh.
- Flow:
  1. When: `lcp-quickstart mainnet reset --force`
  2. Then:
     - Run `cli_mainnet_down` behavior first (best-effort).
     - Delete `workspace.data_dir`.
  3. Error branches:
     - If `--force` is missing: exit non-zero with a warning.
