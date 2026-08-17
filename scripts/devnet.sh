#!/usr/bin/env bash
# One-command local EVM devnet with the Token Zoo deployed and seeded.
#
#   scripts/devnet.sh up      start anvil (state persisted in .devnet/), deploy + seed on first run
#   scripts/devnet.sh down    stop anvil; state is dumped to .devnet/anvil-state.json on exit
#   scripts/devnet.sh reset   down + wipe state + wipe deployments, next `up` starts from genesis
#   scripts/devnet.sh seed    re-run deploy + seed against the running node (fresh addresses)
#   scripts/devnet.sh status  is it running, what is deployed, who has what
#   scripts/devnet.sh logs    tail anvil's log
#
# Everything here uses Anvil's default mnemonic. Those keys are public. Never point this at a real RPC.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVM="$ROOT/contracts/evm"
DEVNET_DIR="$ROOT/.devnet"
STATE_FILE="$DEVNET_DIR/anvil-state.json"
PID_FILE="$DEVNET_DIR/anvil.pid"
LOG_FILE="$DEVNET_DIR/anvil.log"

CHAIN_ID="${CHAIN_ID:-31337}"
PORT="${ANVIL_PORT:-8545}"
RPC_URL="http://127.0.0.1:$PORT"
DEPLOYMENTS="$EVM/deployments/$CHAIN_ID.json"

log() { printf '\033[1;34m[devnet]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[devnet]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing '$1' (install Foundry: https://getfoundry.sh)"; }

is_running() {
  [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

wait_for_rpc() {
  for _ in $(seq 1 60); do
    if cast chain-id --rpc-url "$RPC_URL" >/dev/null 2>&1; then return 0; fi
    sleep 0.25
  done
  die "anvil did not answer on $RPC_URL (see $LOG_FILE)"
}

cmd_up() {
  need anvil; need forge; need cast
  mkdir -p "$DEVNET_DIR"
  if is_running; then
    log "already running (pid $(cat "$PID_FILE")) at $RPC_URL"
  else
    log "starting anvil on $RPC_URL, chain id $CHAIN_ID, state file $STATE_FILE"
    # --state = --load-state + --dump-state: resume if the file exists, dump on exit.
    nohup anvil --chain-id "$CHAIN_ID" --port "$PORT" --state "$STATE_FILE" >"$LOG_FILE" 2>&1 &
    echo $! >"$PID_FILE"
    wait_for_rpc
  fi

  if [[ -f "$DEPLOYMENTS" ]] && [[ -f "$STATE_FILE" ]]; then
    log "state restored, Token Zoo already deployed ($DEPLOYMENTS)"
  else
    cmd_seed
  fi
  cmd_status
}

cmd_seed() {
  need forge
  is_running || die "anvil is not running; run '$0 up' first"
  log "deploying Token Zoo"
  (cd "$EVM" && forge script script/DeployTokenZoo.s.sol --rpc-url "$RPC_URL" --broadcast -q)
  log "seeding world state"
  (cd "$EVM" && forge script script/SeedDevnet.s.sol --rpc-url "$RPC_URL" --broadcast)
}

cmd_down() {
  if is_running; then
    local pid; pid="$(cat "$PID_FILE")"
    log "stopping anvil (pid $pid); state will be dumped to $STATE_FILE"
    kill -INT "$pid"
    for _ in $(seq 1 40); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
    kill -0 "$pid" 2>/dev/null && { log "still alive, sending TERM"; kill -TERM "$pid" || true; }
  else
    log "not running"
  fi
  rm -f "$PID_FILE"
}

cmd_reset() {
  cmd_down
  log "wiping $DEVNET_DIR and $DEPLOYMENTS"
  rm -rf "$DEVNET_DIR" "$DEPLOYMENTS"
}

cmd_status() {
  need cast
  if ! is_running; then log "anvil: not running"; return 0; fi
  log "anvil: running (pid $(cat "$PID_FILE")) at $RPC_URL, block $(cast block-number --rpc-url "$RPC_URL")"
  [[ -f "$DEPLOYMENTS" ]] || { log "deployments: none"; return 0; }
  local usdc usdt payer merchant blacklisted
  usdc="$(jq -r .tokens.USDC.address "$DEPLOYMENTS")"
  usdt="$(jq -r .tokens.USDT.address "$DEPLOYMENTS")"
  payer="$(jq -r .accounts.payer "$DEPLOYMENTS")"
  merchant="$(jq -r .accounts.merchant "$DEPLOYMENTS")"
  blacklisted="$(jq -r .accounts.blacklisted "$DEPLOYMENTS")"
  bal() { cast call "$1" "balanceOf(address)(uint256)" "$2" --rpc-url "$RPC_URL" | awk '{printf "%.0f", $1/1e6}'; }
  log "USDC $usdc  USDT $usdt"
  log "payer       $payer  USDC $(bal "$usdc" "$payer")  USDT $(bal "$usdt" "$payer")"
  log "merchant    $merchant  USDC $(bal "$usdc" "$merchant")  USDT $(bal "$usdt" "$merchant")"
  log "blacklisted $blacklisted  USDT $(bal "$usdt" "$blacklisted") (frozen)"
}

cmd_logs() { tail -n 50 -f "$LOG_FILE"; }

case "${1:-}" in
  up) cmd_up ;;
  down) cmd_down ;;
  reset) cmd_reset ;;
  seed) cmd_seed ;;
  status) cmd_status ;;
  logs) cmd_logs ;;
  *) sed -n '2,10p' "$0"; exit 1 ;;
esac
