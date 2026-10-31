#!/usr/bin/env bash
set -euo pipefail

# Starts all 3 decoupled production surfaces locally:
#   - Surface 1: Standalone 5-Service Polyglot Retail Storefront on http://localhost:3001
#   - Surface 2: Monitoring, Alertmanager & Grafana Golden Signals Preview on http://localhost:3002/monitoring
#   - Surface 3: Clean, Modern AX Observer UI on http://localhost:3000

export PATH="${HOME}/.local/go/bin:/usr/local/go/bin:${HOME}/go/bin:${PATH}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OBSERVER_ADDR="${LISTEN_ADDR:-127.0.0.1:3000}"
STOREFRONT_ADDR="${STOREFRONT_LISTEN_ADDR:-127.0.0.1:3001}"
MONITORING_ADDR="${MONITORING_LISTEN_ADDR:-127.0.0.1:3002}"

cd "${REPO_ROOT}"

echo "==> [ax-bug-bash] Starting 3 decoupled surfaces:"
echo "    - AX Observer UI      : http://${OBSERVER_ADDR}"
echo "    - Retail Storefront   : http://${STOREFRONT_ADDR}"
echo "    - Monitoring / Grafana: http://${MONITORING_ADDR}/monitoring"

if [[ "${1:-}" == "--dry-run" ]]; then
  echo "==> [DRY-RUN] Local 3-surface launcher validated (${OBSERVER_ADDR}, ${STOREFRONT_ADDR}, ${MONITORING_ADDR})."
  exit 0
fi

exec go run ./cmd/ax-observer \
  -addr="${OBSERVER_ADDR}" \
  -storefront-addr="${STOREFRONT_ADDR}" \
  -monitoring-addr="${MONITORING_ADDR}" \
  -all-in-one=true
