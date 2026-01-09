#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${APP_DIR}/../.." && pwd)"

PROTO_ROOT="${REPO_ROOT}/go-lcpd/proto"
PROTO_FILE="${PROTO_ROOT}/lnnode/v1/lnnode.proto"

ESPLORA_BASE_URL="${ESPLORA_BASE_URL:-http://127.0.0.1:3000}"

ALICE_GRPC_ADDR="${ALICE_GRPC_ADDR:-127.0.0.1:10010}"
BOB_GRPC_ADDR="${BOB_GRPC_ADDR:-127.0.0.1:10011}"
ALICE_P2P_ADDR="${ALICE_P2P_ADDR:-127.0.0.1:9736}"
BOB_P2P_ADDR="${BOB_P2P_ADDR:-127.0.0.1:9737}"

FUNDING_AMOUNT_SAT="${FUNDING_AMOUNT_SAT:-200000}"
INVOICE_AMOUNT_MSAT="${INVOICE_AMOUNT_MSAT:-10000}"

require_cmd() {
  local cmd="$1"
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing dependency: ${cmd}" >&2
    if [[ -z "${IN_NIX_SHELL:-}" ]]; then
      cat >&2 <<EOF
hint: run inside the Nix devShell from the repo root:
  cd "${REPO_ROOT}"
  nix develop .#ldk-lcp-node
  bash apps/ldk-lcp-node/scripts/regtest-smoke.sh
EOF
    fi
    exit 1
  fi
}

require_cmd cargo
require_cmd grpcurl
require_cmd jq
require_cmd python3
require_cmd nigiri
require_cmd curl

if [[ ! -f "${PROTO_FILE}" ]]; then
  echo "proto not found: ${PROTO_FILE}" >&2
  exit 1
fi

echo "[1/8] Checking regtest Esplora endpoint: ${ESPLORA_BASE_URL}"
if ! curl -fsS "${ESPLORA_BASE_URL}/blocks/tip/height" >/dev/null; then
  cat >&2 <<EOF
Esplora endpoint not reachable: ${ESPLORA_BASE_URL}

Recommended: start Nigiri (provides Esplora API via Chopsticks on :3000):
  nigiri start
EOF
  exit 1
fi

echo "[2/8] Building ldk-lcp-node"
cd "${APP_DIR}"
cargo build

BIN="${APP_DIR}/target/debug/ldk-lcp-node"
if [[ ! -x "${BIN}" ]]; then
  echo "binary not found after build: ${BIN}" >&2
  exit 1
fi

DATA_ROOT="${APP_DIR}/dev-data/regtest-smoke"
mkdir -p "${DATA_ROOT}"

ALICE_LOG="${DATA_ROOT}/alice.log"
BOB_LOG="${DATA_ROOT}/bob.log"

cleanup() {
  if [[ -n "${ALICE_PID:-}" ]]; then kill "${ALICE_PID}" >/dev/null 2>&1 || true; fi
  if [[ -n "${BOB_PID:-}" ]]; then kill "${BOB_PID}" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

echo "[3/8] Starting 2x ldk-lcp-node"
RUST_LOG="${RUST_LOG:-info}" "${BIN}" \
  --data-dir "${DATA_ROOT}/alice" \
  --grpc-addr "${ALICE_GRPC_ADDR}" \
  --p2p-listen "${ALICE_P2P_ADDR}" \
  --network regtest \
  --esplora-base-url "${ESPLORA_BASE_URL}" \
  >"${ALICE_LOG}" 2>&1 &
ALICE_PID="$!"

RUST_LOG="${RUST_LOG:-info}" "${BIN}" \
  --data-dir "${DATA_ROOT}/bob" \
  --grpc-addr "${BOB_GRPC_ADDR}" \
  --p2p-listen "${BOB_P2P_ADDR}" \
  --network regtest \
  --esplora-base-url "${ESPLORA_BASE_URL}" \
  >"${BOB_LOG}" 2>&1 &
BOB_PID="$!"

grpc() {
  local addr="$1"
  shift
  grpcurl -plaintext -format json \
    -import-path "${PROTO_ROOT}" \
    -proto "${PROTO_FILE}" \
    "${addr}" "$@"
}

wait_grpc() {
  local addr="$1"
  local deadline_s="${2:-30}"
  local start
  start="$(date +%s)"
  while true; do
    if grpc "${addr}" -d '{}' lnnode.v1.LightningNodeService/GetNodeInfo >/dev/null 2>&1; then
      return 0
    fi
    if (( "$(date +%s)" - start > deadline_s )); then
      echo "gRPC not ready: ${addr} (timed out after ${deadline_s}s)" >&2
      return 1
    fi
    sleep 0.5
  done
}

echo "[4/8] Waiting for gRPC readiness"
wait_grpc "${ALICE_GRPC_ADDR}"
wait_grpc "${BOB_GRPC_ADDR}"

ALICE_PUBKEY_B64="$(grpc "${ALICE_GRPC_ADDR}" -d '{}' lnnode.v1.LightningNodeService/GetNodeInfo | jq -r .nodePubkey)"
BOB_PUBKEY_B64="$(grpc "${BOB_GRPC_ADDR}" -d '{}' lnnode.v1.LightningNodeService/GetNodeInfo | jq -r .nodePubkey)"

echo "[5/8] Funding Alice wallet via nigiri faucet"
ALICE_ONCHAIN_ADDR="$(grpc "${ALICE_GRPC_ADDR}" -d '{}' lnnode.v1.LightningNodeService/NewAddress | jq -r .address)"
nigiri faucet "${ALICE_ONCHAIN_ADDR}" >/dev/null

# Ensure enough confirmations for channel funding (LDK default is typically >1).
MINER_ADDR="$(nigiri rpc getnewaddress "" "bech32" | tr -d '\n')"
nigiri rpc generatetoaddress 6 "${MINER_ADDR}" >/dev/null

echo "[6/8] Connecting peers (Alice -> Bob)"
grpc "${ALICE_GRPC_ADDR}" -d "{\"peerPubkey\":\"${BOB_PUBKEY_B64}\",\"addr\":\"${BOB_P2P_ADDR}\"}" \
  lnnode.v1.LightningNodeService/ConnectPeer >/dev/null

echo "[7/8] Opening channel"
grpc "${ALICE_GRPC_ADDR}" -d "{\"peerPubkey\":\"${BOB_PUBKEY_B64}\",\"localFundingAmountSat\":${FUNDING_AMOUNT_SAT},\"announceChannel\":false}" \
  lnnode.v1.LightningNodeService/OpenChannel >/dev/null

nigiri rpc generatetoaddress 6 "${MINER_ADDR}" >/dev/null

echo "Waiting for channel to become usable..."
deadline="$(($(date +%s) + 60))"
while true; do
  usable_count="$(grpc "${ALICE_GRPC_ADDR}" -d '{}' lnnode.v1.LightningNodeService/ListChannels | jq '[.channels[] | select(.usable == true)] | length')"
  if [[ "${usable_count}" -ge 1 ]]; then
    break
  fi
  if (( "$(date +%s)" > deadline )); then
    echo "channel did not become usable within 60s" >&2
    exit 1
  fi
  sleep 1
done

echo "[8/8] Invoice + payment"
DESC_HASH_B64="$(python3 -c 'import os,base64; print(base64.b64encode(os.urandom(32)).decode())')"
INV_JSON="$(grpc "${BOB_GRPC_ADDR}" -d "{\"descriptionHash\":\"${DESC_HASH_B64}\",\"amountMsat\":${INVOICE_AMOUNT_MSAT},\"expirySeconds\":3600}" \
  lnnode.v1.LightningNodeService/CreateInvoice)"
PAYMENT_REQUEST="$(echo "${INV_JSON}" | jq -r .paymentRequest)"
PAYMENT_HASH_B64="$(echo "${INV_JSON}" | jq -r .paymentHash)"

PAY_JSON="$(grpc "${ALICE_GRPC_ADDR}" -d "{\"paymentRequest\":\"${PAYMENT_REQUEST}\",\"timeoutSeconds\":30,\"feeLimitMsat\":1000}" \
  lnnode.v1.LightningNodeService/PayInvoice)"
PAY_STATUS="$(echo "${PAY_JSON}" | jq -r .status)"
if [[ "${PAY_STATUS}" != "STATUS_SUCCEEDED" ]]; then
  echo "payment failed: ${PAY_JSON}" >&2
  exit 1
fi

SETTLE_JSON="$(grpc "${BOB_GRPC_ADDR}" -d "{\"paymentHash\":\"${PAYMENT_HASH_B64}\",\"timeoutSeconds\":30}" \
  lnnode.v1.LightningNodeService/WaitInvoiceSettled)"
SETTLE_STATE="$(echo "${SETTLE_JSON}" | jq -r .state)"
if [[ "${SETTLE_STATE}" != "STATE_SETTLED" ]]; then
  echo "invoice not settled: ${SETTLE_JSON}" >&2
  exit 1
fi

echo "OK: peer/channel/invoice/payment smoke test passed"
echo "logs:"
echo "  alice: ${ALICE_LOG}"
echo "  bob:   ${BOB_LOG}"
