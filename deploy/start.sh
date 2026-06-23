#!/usr/bin/env bash
# Start (or stop) the Sandock single-host backend: host-agent + API.
#
# Prerequisites (one-time):
#   sudo bash deploy/linux-host-setup.sh
#   download kernel + rootfs to /var/lib/sandock/images/
#   sudo bash deploy/inject-vm-agent.sh bin/vm-agent /var/lib/sandock/images/base-rootfs.ext4
#   sudo cp deploy/config.example.yaml /etc/sandock/config.yaml  # then edit secrets/paths
#   make build
#
# Usage:
#   bash deploy/start.sh          # start host-agent + api
#   bash deploy/start.sh stop     # stop both
#   bash deploy/start.sh status   # show process + health state
#
# Environment overrides:
#   SANDOCK_CONFIG=/etc/sandock/config.yaml
#   SANDOCK_LOG_DIR=/var/log/sandock

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CONFIG="${SANDOCK_CONFIG:-/etc/sandock/config.yaml}"
LOG_DIR="${SANDOCK_LOG_DIR:-/var/log/sandock}"
RUN_DIR="/var/run/sandock"
HOST_AGENT_PID="${RUN_DIR}/host-agent.pid"
API_PID="${RUN_DIR}/api.pid"

HOST_AGENT_BIN="${REPO_ROOT}/bin/host-agent"
API_BIN="${REPO_ROOT}/bin/api"

mkdir -p "${LOG_DIR}" "${RUN_DIR}" 2>/dev/null || {
  LOG_DIR="/tmp/sandock"
  RUN_DIR="/tmp/sandock/run"
  HOST_AGENT_PID="${RUN_DIR}/host-agent.pid"
  API_PID="${RUN_DIR}/api.pid"
  mkdir -p "${LOG_DIR}" "${RUN_DIR}"
}

log() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

read_config_val() {
  local key="$1"
  grep -E "^\s*${key}:" "$CONFIG" 2>/dev/null | head -1 \
    | sed -E 's/^[^:]*:[[:space:]]*"?([^"#]+)"?.*/\1/' \
    | tr -d ' "' \
    | xargs || true
}

stop_services() {
  log "Stopping Sandock services..."
  if [ -f "${API_PID}" ] && kill -0 "$(cat "${API_PID}")" 2>/dev/null; then
    kill "$(cat "${API_PID}")" 2>/dev/null || true
  fi
  pkill -f "${HOST_AGENT_BIN}" 2>/dev/null || true
  pkill -f "${API_BIN}" 2>/dev/null || true
  rm -f "${HOST_AGENT_PID}" "${API_PID}"
  sleep 1
  log "Stopped."
}

show_status() {
  echo "Config:     ${CONFIG}"
  echo "Repo:       ${REPO_ROOT}"
  echo "Logs:       ${LOG_DIR}/"
  echo ""
  if pgrep -f "${HOST_AGENT_BIN}" >/dev/null 2>&1; then
    echo "host-agent: running (pid $(pgrep -f "${HOST_AGENT_BIN}" | head -1))"
  else
    echo "host-agent: not running"
  fi
  if pgrep -f "${API_BIN}" >/dev/null 2>&1; then
    echo "api:        running (pid $(pgrep -f "${API_BIN}" | head -1))"
  else
    echo "api:        not running"
  fi
  echo ""
  local listen
  listen="$(read_config_val listen_addr)"
  listen="${listen:-:8080}"
  if [[ "${listen}" != :* ]] && [[ "${listen}" != *://* ]]; then
    listen="http://${listen}"
  elif [[ "${listen}" == :* ]]; then
    listen="http://127.0.0.1${listen}"
  fi
  curl -sf "${listen}/healthz" 2>/dev/null | jq . 2>/dev/null || echo "API health: unreachable at ${listen}/healthz"
}

preflight() {
  [ -f "${CONFIG}" ] || die "config not found: ${CONFIG} (copy deploy/config.example.yaml)"
  [ -x "${HOST_AGENT_BIN}" ] || die "host-agent binary missing — run: make build"
  [ -x "${API_BIN}" ] || die "api binary missing — run: make build"
  [ -c /dev/kvm ] || die "/dev/kvm not found — need nested-virt-capable EC2 instance"

  local kernel rootfs
  kernel="$(read_config_val kernel_image)"
  rootfs="$(read_config_val base_rootfs)"
  [ -n "${kernel}" ] && [ -f "${kernel}" ] || die "kernel not found: ${kernel:-<unset in config>}"
  [ -n "${rootfs}" ] && [ -f "${rootfs}" ] || die "base rootfs not found: ${rootfs:-<unset in config>}"

  if grep -q '^host_agents:' "${CONFIG}" 2>/dev/null; then
    if grep -A1 '^host_agents:' "${CONFIG}" | grep -q 'http_addr:'; then
      die "remove active host_agents: block from ${CONFIG} for single-host mode"
    fi
  fi

  # cgroup parent for sandboxes
  if [ -d /sys/fs/cgroup/sandboxes ] || [ -w /sys/fs/cgroup ]; then
    sudo mkdir -p /sys/fs/cgroup/sandboxes 2>/dev/null || true
    echo '+cpu +memory' | sudo tee /sys/fs/cgroup/sandboxes/cgroup.subtree_control >/dev/null 2>&1 || true
  fi
}

start_services() {
  preflight
  stop_services

  log "Starting host-agent (requires root for Firecracker/KVM)..."
  sudo truncate -s 0 "${LOG_DIR}/host-agent.log" 2>/dev/null || true
  sudo bash -c "cd '${REPO_ROOT}' && nohup '${HOST_AGENT_BIN}' --config '${CONFIG}' >> '${LOG_DIR}/host-agent.log' 2>&1 & echo \$! > '${HOST_AGENT_PID}'"

  sleep 1
  pgrep -f "${HOST_AGENT_BIN}" >/dev/null || die "host-agent failed to start — see ${LOG_DIR}/host-agent.log"

  log "Starting API..."
  truncate -s 0 "${LOG_DIR}/api.log" 2>/dev/null || true
  cd "${REPO_ROOT}"
  nohup "${API_BIN}" --config "${CONFIG}" >> "${LOG_DIR}/api.log" 2>&1 &
  echo $! > "${API_PID}"

  sleep 1
  pgrep -f "${API_BIN}" >/dev/null || die "api failed to start — see ${LOG_DIR}/api.log"

  local listen jwt
  listen="$(read_config_val listen_addr)"
  listen="${listen:-:8080}"
  if [[ "${listen}" == :* ]]; then
    listen="http://127.0.0.1${listen}"
  elif [[ "${listen}" != http* ]]; then
    listen="http://${listen}"
  fi

  jwt="$(read_config_val jwt_secret)"
  for i in $(seq 1 15); do
    if curl -sf "${listen}/healthz" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done

  echo ""
  echo "Sandock is up."
  echo ""
  echo "  API URL:    ${listen}"
  echo "  API token:  ${jwt:-<set api.jwt_secret in config>}"
  echo ""
  echo "  export SANDOCK_URL=${listen}"
  echo "  export SANDOCK_TOKEN=\"${jwt}\""
  echo ""
  echo "  Create sandbox:"
  echo "  curl -s -X POST \"\$SANDOCK_URL/v1/sandboxes\" \\"
  echo "    -H \"Authorization: Bearer \$SANDOCK_TOKEN\" \\"
  echo "    -H \"Content-Type: application/json\" \\"
  echo "    -d '{\"image\":\"base\",\"cpu_millis\":500,\"memory_mib\":512,\"timeout_ms\":300000}' | jq ."
  echo ""
  echo "  Logs:  ${LOG_DIR}/host-agent.log  ${LOG_DIR}/api.log"
  echo "  Stop:  bash deploy/start.sh stop"
}

case "${1:-start}" in
  start) start_services ;;
  stop)  stop_services ;;
  status) show_status ;;
  restart) stop_services; start_services ;;
  *)
    echo "Usage: $0 {start|stop|status|restart}"
    exit 1
    ;;
esac
