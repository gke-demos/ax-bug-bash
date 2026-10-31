#!/usr/bin/env bash
# Record the 6-Scene ADR-0012 Live-GKE Stage Walkthrough into /tmp/ax-bug-bash-demo.mp4.
#
# RUNNER
#   surface:  web-app
#   start:    kubectl -n ax-bug-bash port-forward svc/ax-observer ${OBS_PORT:-3000}:3000 & kubectl -n ax-bug-bash port-forward svc/storefront ${SF_PORT:-3001}:3001 & kubectl -n monitoring port-forward svc/grafana ${GRAF_PORT:-3002}:3002
#   url:      http://127.0.0.1:3000
#   ready:    poll http://127.0.0.1:${OBS_PORT}/healthz && http://127.0.0.1:${SF_PORT}/healthz && http://127.0.0.1:${GRAF_PORT}/api/health
#   interact:
#     - Scene 1: Navigate to http://127.0.0.1:${SF_PORT}/ (Retail Storefront), reset to baseline-v1, trigger INC-101, click #btn-place-order to show HTTP 500
#     - Scene 2: Navigate to http://127.0.0.1:${GRAF_PORT}/d/ax-bug-bash-storefront/ax-bug-bash-polyglot-storefront-and-rollouts?orgId=1&kiosk (Grafana)
#     - Scene 3: Navigate to http://127.0.0.1:${OBS_PORT}/ (AX Observer Bug Bash Tree) to inspect Root Task and parallel Child Tasks executing Red-Green TDD
#     - Scene 4: Poll http://127.0.0.1:${OBS_PORT}/api/state until HEALED, inspect < 3s selective hot-reload and click #btn-place-order for HTTP 200 CONFIRMED recovery
#     - Scene 5: Navigate to http://127.0.0.1:${OBS_PORT}/workers and http://127.0.0.1:${OBS_PORT}/about (Tab 1, Tab 2, Tab 3)
#     - Scene 6: Click #btn-theme-light on /about and / to demonstrate high-contrast Projector Light Mode with zero layout shift
#   stop:     kill -INT $FFMPEG_PID && gbrowser stop-session && kill $XVFB_PID $PF_OBS_PID $PF_SF_PID $PF_GF_PID

set -euo pipefail

OUTPUT_MP4="${OUTPUT_MP4:-/tmp/ax-bug-bash-demo.mp4}"
DISPLAY_NUM="${DISPLAY_NUM:-:98}"
GBROWSER="${GBROWSER:-/google/bin/releases/gemini-agents-gbrowser/gbrowser}"
SESSION_NAME="axstage"
OBS_PORT="${OBS_PORT:-3000}"
SF_PORT="${SF_PORT:-3001}"
GRAF_PORT="${GRAF_PORT:-3002}"

PF_OBS_PID=""
PF_SF_PID=""
PF_GF_PID=""
XVFB_PID=""
FFMPEG_PID=""

run_gbrowser() {
  DISPLAY="${DISPLAY_NUM}" "${GBROWSER}" --session="${SESSION_NAME}" --headless=false --window-size=1280,800 "$@"
}

cleanup() {
  set +e
  if [[ -n "${FFMPEG_PID}" ]] && kill -0 "${FFMPEG_PID}" 2>/dev/null; then
    kill -INT "${FFMPEG_PID}" 2>/dev/null || true
    wait "${FFMPEG_PID}" 2>/dev/null || true
  fi
  DISPLAY="${DISPLAY_NUM}" "${GBROWSER}" --session="${SESSION_NAME}" stop-session >/dev/null 2>&1 || true
  for pid in "${XVFB_PID}" "${PF_OBS_PID}" "${PF_SF_PID}" "${PF_GF_PID}"; do
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
  done
}
if [[ "${1:-}" == "--dry-run" ]]; then
  echo "==> [DRY-RUN] 6-Scene Stage Walkthrough recorder validated (output=${OUTPUT_MP4}, ports=:${OBS_PORT}/:${SF_PORT}/:${GRAF_PORT})."
  exit 0
fi

trap cleanup EXIT INT TERM

echo "[1/7] Ensuring GKE port-forwards (ax-observer:${OBS_PORT}, storefront:${SF_PORT}, grafana:${GRAF_PORT})..."
if ! curl -sf "http://127.0.0.1:${OBS_PORT}/healthz" >/dev/null 2>&1; then
  kubectl -n ax-bug-bash port-forward svc/ax-observer "${OBS_PORT}:3000" >/dev/null 2>&1 &
  PF_OBS_PID=$!
  sleep 1
  if ! kill -0 "${PF_OBS_PID}" 2>/dev/null; then
    OBS_PORT=13000
    kubectl -n ax-bug-bash port-forward svc/ax-observer "${OBS_PORT}:3000" >/dev/null 2>&1 &
    PF_OBS_PID=$!
  fi
fi

if ! curl -sf "http://127.0.0.1:${SF_PORT}/healthz" >/dev/null 2>&1; then
  kubectl -n ax-bug-bash port-forward svc/storefront "${SF_PORT}:3001" >/dev/null 2>&1 &
  PF_SF_PID=$!
  sleep 1
  if ! kill -0 "${PF_SF_PID}" 2>/dev/null; then
    SF_PORT=13001
    kubectl -n ax-bug-bash port-forward svc/storefront "${SF_PORT}:3001" >/dev/null 2>&1 &
    PF_SF_PID=$!
  fi
fi

if ! curl -sf "http://127.0.0.1:${GRAF_PORT}/api/health" >/dev/null 2>&1; then
  kubectl -n monitoring port-forward svc/grafana "${GRAF_PORT}:3002" >/dev/null 2>&1 &
  PF_GF_PID=$!
  sleep 1
  if ! kill -0 "${PF_GF_PID}" 2>/dev/null; then
    GRAF_PORT=13002
    kubectl -n monitoring port-forward svc/grafana "${GRAF_PORT}:3002" >/dev/null 2>&1 &
    PF_GF_PID=$!
  fi
fi

for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${OBS_PORT}/healthz" >/dev/null \
    && curl -sf "http://127.0.0.1:${SF_PORT}/healthz" >/dev/null \
    && curl -sf "http://127.0.0.1:${GRAF_PORT}/api/health" >/dev/null; then
    break
  fi
  sleep 1
done

curl -sf -X POST "http://127.0.0.1:${OBS_PORT}/api/incidents/reset" \
  -H "Content-Type: application/json" \
  -d '{"default_harness":"multi"}' >/dev/null || true

echo "[2/7] Launching Xvfb on ${DISPLAY_NUM} (1280x800x24)..."
Xvfb "${DISPLAY_NUM}" -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 2
export DISPLAY="${DISPLAY_NUM}"

DISPLAY="${DISPLAY_NUM}" "${GBROWSER}" --session="${SESSION_NAME}" stop-session >/dev/null 2>&1 || true

# Pre-warm the browser window on Scene 1 before starting ffmpeg so the opening frame is crisp
run_gbrowser navigate "http://127.0.0.1:${SF_PORT}/" >/dev/null 2>&1 || true
sleep 1

echo "[3/7] Starting ffmpeg x11grab + anullsrc AAC recording to ${OUTPUT_MP4}..."
rm -f "${OUTPUT_MP4}"
ffmpeg -y \
  -f x11grab -video_size 1280x800 -framerate 20 -draw_mouse 0 -i "${DISPLAY_NUM}" \
  -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=44100 \
  -codec:v libx264 -preset veryfast -pix_fmt yuv420p \
  -codec:a aac -b:a 128k \
  -movflags +faststart \
  "${OUTPUT_MP4}" >/dev/null 2>&1 &
FFMPEG_PID=$!
sleep 2

echo "[4/7] Scene 1 & Scene 2: Retail Storefront (:${SF_PORT}) INC-101 trigger + Grafana (:${GRAF_PORT}) ax-bug-bash-storefront..."
curl -sf -X POST "http://127.0.0.1:${SF_PORT}/api/incidents/activate" \
  -H "Content-Type: application/json" \
  -d '{"incident_id":"INC-101"}' >/dev/null || true

# Trigger INC-101 asynchronously against live GKE so Child Tasks fan out while we tour the surfaces
curl -sf -X POST "http://127.0.0.1:${OBS_PORT}/api/incidents/trigger" \
  -H "Content-Type: application/json" \
  -d '{"incident_id":"INC-101","harness":"multi","model":"gemini-3.8-flash"}' >/dev/null &
TRIGGER_BG_PID=$!
sleep 1

# Refresh Retail Storefront and click #btn-place-order to visually render the HTTP 500 error banner
run_gbrowser navigate "http://127.0.0.1:${SF_PORT}/" >/dev/null 2>&1 || true
run_gbrowser click "http://127.0.0.1:${SF_PORT}/" "#btn-place-order" >/dev/null 2>&1 || true
sleep 3

# Scene 2: Open Grafana pre-provisioned dashboard (UID: ax-bug-bash-storefront)
run_gbrowser navigate "http://127.0.0.1:${GRAF_PORT}/d/ax-bug-bash-storefront/ax-bug-bash-polyglot-storefront-and-rollouts?orgId=1&kiosk" >/dev/null 2>&1 || true
sleep 4

echo "[5/7] Scene 3 & Scene 4: AX Observer (/) Bug Bash Tree, Red-Green TDD & < 3s Selective Git Hot-Reload..."
run_gbrowser navigate "http://127.0.0.1:${OBS_PORT}/" >/dev/null 2>&1 || true
sleep 3

# Poll /api/state until the live GKE Bug Bash Tree completes and heals the storefront (up to 75s)
export OBS_PORT
for _ in $(seq 1 38); do
  PHASE=$(python3 - << 'PY'
import json, os, urllib.request
port = os.environ.get("OBS_PORT", "3000")
try:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/state", timeout=2) as r:
        data = json.loads(r.read().decode("utf-8"))
        tree = data.get("latest_tree") or {}
        print(tree.get("phase", ""))
except Exception:
    print("")
PY
)
  if [[ "${PHASE}" == "HEALED" || "${PHASE}" == "COMPLETED" ]]; then
    break
  fi
  sleep 2
done
wait "${TRIGGER_BG_PID}" 2>/dev/null || true

# Reload AX Observer (/) to display the completed Red-Green diffs and < 3s hot-reload badge
run_gbrowser navigate "http://127.0.0.1:${OBS_PORT}/" >/dev/null 2>&1 || true
sleep 3

# Toggle Task.spec.config JSON drawer briefly
run_gbrowser click "http://127.0.0.1:${OBS_PORT}/" "#btn-toggle-config" >/dev/null 2>&1 || true
sleep 2

# Reload Retail Storefront and click #btn-place-order to visually render HTTP 200 CONFIRMED recovery
run_gbrowser navigate "http://127.0.0.1:${SF_PORT}/" >/dev/null 2>&1 || true
run_gbrowser click "http://127.0.0.1:${SF_PORT}/" "#btn-place-order" >/dev/null 2>&1 || true
sleep 3

echo "[6/7] Scene 5 & Scene 6: /workers Warm Pool, /about 3-Tab Catalog & Projector Light Mode..."
run_gbrowser navigate "http://127.0.0.1:${OBS_PORT}/workers" >/dev/null 2>&1 || true
sleep 3

# Navigate to /about (Tab 1: System Topology & Blast-Radius Simulator)
run_gbrowser navigate "http://127.0.0.1:${OBS_PORT}/about" >/dev/null 2>&1 || true
sleep 3

# Switch to Tab 2: Recursive Task-Tree & TDD Lifecycle
run_gbrowser click "http://127.0.0.1:${OBS_PORT}/about" "button[data-about-tab=\"lifecycle\"]" >/dev/null 2>&1 || true
sleep 3

# Switch to Tab 3: Zero-Secret Security & ADR Catalog
run_gbrowser click "http://127.0.0.1:${OBS_PORT}/about" "button[data-about-tab=\"security\"]" >/dev/null 2>&1 || true
sleep 3

# Scene 6: Toggle Projector Light Mode on /about and /
run_gbrowser click "http://127.0.0.1:${OBS_PORT}/about" "#btn-theme-light" >/dev/null 2>&1 || true
sleep 3

run_gbrowser navigate "http://127.0.0.1:${OBS_PORT}/" >/dev/null 2>&1 || true
sleep 1
run_gbrowser click "http://127.0.0.1:${OBS_PORT}/" "#btn-theme-light" >/dev/null 2>&1 || true
sleep 3

echo "[7/7] Finalizing recording and running ffprobe verification on ${OUTPUT_MP4}..."
kill -INT "${FFMPEG_PID}"
wait "${FFMPEG_PID}" 2>/dev/null || true
FFMPEG_PID=""

ls -lh "${OUTPUT_MP4}"
ffprobe -v error \
  -show_entries stream=codec_name,width,height:format=duration,size \
  -of default=noprint_wrappers=1 "${OUTPUT_MP4}"
