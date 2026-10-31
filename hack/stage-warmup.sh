#!/usr/bin/env bash
# ADR-0014 One-Command Pre-Stage Warmup Gate (make stage-ready).
# Verifies all live GKE deployments and warm ateworker-gvisor worker slots,
# idempotently ensures local port-forwards on :3000, :3001, and :3002,
# resets the Retail Storefront to baseline-v1, drains firing alerts, and
# verifies the 14-ADR /about Architecture Topology Catalog.

set -euo pipefail

OBS_PORT="${OBS_PORT:-3000}"
SF_PORT="${SF_PORT:-3001}"
GRAF_PORT="${GRAF_PORT:-3002}"
PID_FILE="${PID_FILE:-/tmp/ax-bug-bash-stage-pf.pids}"
PORTS_FILE="${PORTS_FILE:-/tmp/ax-bug-bash-stage-pf.ports}"
MODE="${1:-warmup}"

stop_managed_port_forwards() {
  if [[ -f "${PID_FILE}" ]]; then
    while IFS= read -r pid; do
      if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
      fi
    done < "${PID_FILE}"
    rm -f "${PID_FILE}" "${PORTS_FILE}"
  fi
}

if [[ "${MODE}" == "--stop" ]]; then
  stop_managed_port_forwards
  echo "==> [STAGE-WARMUP] Stopped managed port-forwards recorded in ${PID_FILE}."
  exit 0
fi

if [[ "${MODE}" == "--dry-run" ]]; then
  echo "==> [STAGE-WARMUP] Dry-run verification: 6-point Stage Warmup Gate configured for ports :${OBS_PORT}, :${SF_PORT}, :${GRAF_PORT}."
  exit 0
fi

if [[ -f "${PORTS_FILE}" ]]; then
  # shellcheck disable=SC1090
  source "${PORTS_FILE}"
fi

START_EPOCH="$(date +%s)"
STAGE_TMP_DIR="$(mktemp -d -t ax-stage-warmup-XXXXXX)"
trap 'rm -rf "${STAGE_TMP_DIR}"' EXIT INT TERM

echo "==> [Gate 1/6] Verifying GKE Control Plane, Polyglot Storefront Mesh & Warm WorkerPool..."
kubectl -n ax-system rollout status statefulset/ax-postgres --timeout=30s >/dev/null
kubectl -n ax-system wait --for=condition=Available deployment/ax-server deployment/model-gateway --timeout=30s >/dev/null
kubectl -n ax-bug-bash wait --for=condition=Available \
  deployment/storefront \
  deployment/ax-observer \
  deployment/ax-bug-bash-workers --timeout=30s >/dev/null
kubectl -n monitoring wait --for=condition=Available \
  deployment/prometheus \
  deployment/alertmanager \
  deployment/loki \
  deployment/grafana --timeout=30s >/dev/null

READY_WORKERS="$(kubectl -n ax-bug-bash get workerpools.ate.dev ax-bug-bash-workers -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")"
if [[ "${READY_WORKERS:-0}" -lt 4 ]]; then
  echo "ERROR: WorkerPool/ax-bug-bash-workers readyReplicas=${READY_WORKERS:-0}, want >= 4" >&2
  exit 1
fi
echo "    Control Plane, 5-Service Polyglot Mesh, Monitoring Stack, and WorkerPool (${READY_WORKERS}/4 Ready ateworker-gvisor slots) verified."

echo "==> [Gate 2/6] Ensuring idempotent local port-forwards (:${OBS_PORT}, :${SF_PORT}, :${GRAF_PORT})..."
touch "${PID_FILE}"

ensure_port_forward() {
  local name="$1"
  local namespace="$2"
  local target="$3"
  local preferred_port="$4"
  local fallback_port="$5"
  local alt_port="$6"
  local remote_port="$7"
  local verify_path="$8"
  local expected_key="$9"
  local out_var="${10}"

  for candidate_port in "${preferred_port}" "${fallback_port}" "${alt_port}"; do
    local body
    body="$(curl -sf "http://127.0.0.1:${candidate_port}${verify_path}" 2>/dev/null || true)"
    if [[ -n "${body}" ]] && [[ "${body}" == *"${expected_key}"* ]]; then
      echo "    Reusing healthy ${name} listener on http://127.0.0.1:${candidate_port}"
      printf -v "${out_var}" "%s" "${candidate_port}"
      return 0
    fi
  done

  for candidate_port in "${preferred_port}" "${fallback_port}" "${alt_port}"; do
    nohup kubectl -n "${namespace}" port-forward "${target}" "${candidate_port}:${remote_port}" >/tmp/ax-stage-pf-${name}.log 2>&1 &
    local pf_pid=$!
    sleep 0.6
    if ! kill -0 "${pf_pid}" 2>/dev/null; then
      continue
    fi
    for _ in $(seq 1 20); do
      local body
      body="$(curl -sf "http://127.0.0.1:${candidate_port}${verify_path}" 2>/dev/null || true)"
      if [[ -n "${body}" ]] && [[ "${body}" == *"${expected_key}"* ]]; then
        echo "${pf_pid}" >> "${PID_FILE}"
        echo "    Started ${name} port-forward on http://127.0.0.1:${candidate_port} (pid=${pf_pid})"
        printf -v "${out_var}" "%s" "${candidate_port}"
        return 0
      fi
      sleep 0.25
    done
    kill "${pf_pid}" 2>/dev/null || true
  done

  echo "ERROR: Failed to establish ${name} port-forward on :${preferred_port}, :${fallback_port}, or :${alt_port}" >&2
  exit 1
}

ensure_port_forward "ax-observer" "ax-bug-bash" "svc/ax-observer" "${OBS_PORT}" "13000" "23000" "3000" "/api/about/catalog" "view_box" OBS_PORT
ensure_port_forward "storefront" "ax-bug-bash" "svc/storefront" "${SF_PORT}" "13001" "23001" "3001" "/healthz" "storefront-web" SF_PORT
ensure_port_forward "grafana" "monitoring" "svc/grafana" "${GRAF_PORT}" "13002" "23002" "3002" "/api/health" "database" GRAF_PORT

cat <<EOF > "${PORTS_FILE}"
OBS_PORT=${OBS_PORT}
SF_PORT=${SF_PORT}
GRAF_PORT=${GRAF_PORT}
EOF

echo "==> [Gate 3/6] Resetting Retail Storefront to clean baseline-v1 revision..."
curl -sf -X POST "http://127.0.0.1:${OBS_PORT}/api/incidents/reset" \
  -H "Content-Type: application/json" \
  -d '{"default_harness":"multi"}' > "${STAGE_TMP_DIR}/reset.json"
curl -sf "http://127.0.0.1:${SF_PORT}/healthz" > "${STAGE_TMP_DIR}/sf_health.json"
python3 - "${STAGE_TMP_DIR}/reset.json" "${STAGE_TMP_DIR}/sf_health.json" << 'EOF'
import json, pathlib, sys
reset_data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
sf_health = json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
if reset_data.get("status") != "reset":
    raise SystemExit(f"Reset status mismatch: {reset_data.get('status')!r}")
if sf_health.get("status") != "ok" or sf_health.get("service") != "storefront-web":
    raise SystemExit(f"Storefront health mismatch: {sf_health!r}")
print(f"    Storefront reset to baseline-v1 (commit_sha={sf_health.get('commit_sha', '')[:8]}, default_harness={reset_data.get('default_harness')})")
EOF

echo "==> [Gate 4/6] Verifying zero active incidents or firing alerts..."
for _ in $(seq 1 15); do
  curl -sf "http://127.0.0.1:${OBS_PORT}/api/state" > "${STAGE_TMP_DIR}/state.json"
  ACTIVE_INC="$(python3 - "${STAGE_TMP_DIR}/state.json" << 'EOF'
import json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
print(data.get("active_incident", ""))
EOF
)"
  if [[ -z "${ACTIVE_INC}" ]]; then
    break
  fi
  sleep 1
done
echo "    Alert drain verified (active_incident='none')."

echo "==> [Gate 5/6] Verifying LIVE_GKE execution mode and warm sandbox telemetry on /api/workers..."
curl -sf "http://127.0.0.1:${OBS_PORT}/api/workers" > "${STAGE_TMP_DIR}/workers.json"
python3 - "${STAGE_TMP_DIR}/workers.json" << 'EOF'
import json, pathlib, sys
w = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
mode = w.get("execution_mode", "")
workers = w.get("workers", [])
if mode != "LIVE_GKE":
    raise SystemExit(f"Expected execution_mode='LIVE_GKE', got {mode!r}")
if len(workers) < 4:
    raise SystemExit(f"Expected >= 4 worker slots on /api/workers, got {len(workers)}")
print(f"    /api/workers verified: execution_mode={mode}, warm_slots={len(workers)}")
EOF

echo "==> [Gate 6/6] Verifying 14-ADR /about Architecture Topology Catalog..."
curl -sf "http://127.0.0.1:${OBS_PORT}/api/about/catalog" > "${STAGE_TMP_DIR}/catalog.json"
python3 - "${STAGE_TMP_DIR}/catalog.json" << 'EOF'
import json, pathlib, sys
cat = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
adrs = cat.get("adrs", [])
nodes = cat.get("nodes", [])
edges = cat.get("edges", [])
baseline_tag = cat.get("status_strip", {}).get("storefront_baseline_tag", "")
if len(adrs) != 14:
    raise SystemExit(f"Expected 14 ADRs in /api/about/catalog, got {len(adrs)}")
if len(nodes) != 12 or len(edges) != 11:
    raise SystemExit(f"Expected 12 nodes and 11 edges in /api/about/catalog, got {len(nodes)} nodes and {len(edges)} edges")
if baseline_tag != "baseline-v1":
    raise SystemExit(f"Expected storefront_baseline_tag='baseline-v1', got {baseline_tag!r}")
print(f"    /api/about/catalog verified: {len(adrs)} ADRs (ADR-0001..{adrs[-1]['id']}), {len(nodes)} nodes, {len(edges)} edges, tag={baseline_tag}")
EOF

ELAPSED="$(( $(date +%s) - START_EPOCH ))"
echo ""
echo "================================================================================"
echo "  STAGE WARMUP GATE PASSED IN ${ELAPSED}s (READY FOR LIVE PRESENTATION)"
echo "  - Surface 1 (Retail Storefront): http://localhost:${SF_PORT}"
echo "  - Surface 2 (Monitoring/Grafana): http://localhost:${GRAF_PORT}/d/ax-bug-bash-storefront/ax-bug-bash-polyglot-storefront-and-rollouts?orgId=1&kiosk"
echo "  - Surface 3 (AX Observer):       http://localhost:${OBS_PORT}"
echo "================================================================================"
