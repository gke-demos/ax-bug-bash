#!/usr/bin/env bash
set -euo pipefail

# Builds the patched google/ax (4b7bf6f) ax-server binary with Agent Substrate v0.6.1+
# (substrate.ateapi.v1.Control) compatibility, context.WithoutCancel SuspendTask protection,
# /healthz wakeup probing, GOLDEN_SNAPSHOT_MODE_DISABLED, durable /workspace/.ax marker,
# and automatic per-actor cluster-internal EgressPolicy creation (AX_AUTO_ACTOR_EGRESS=true).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
OUT_BIN="${1:-${REPO_ROOT}/bin/linux_amd64/ax-server}"
PATCH_FILE="${SCRIPT_DIR}/ax-server-v061.patch"

AX_UPSTREAM_DIR="${AX_UPSTREAM_DIR:-}"
if [[ -z "${AX_UPSTREAM_DIR}" ]]; then
  if [[ -d "${REPO_ROOT}/../google/ax" ]]; then
    AX_UPSTREAM_DIR="$(cd "${REPO_ROOT}/../google/ax" && pwd)"
  elif [[ -d "${HOME}/Documents/github.com/google/ax" ]]; then
    AX_UPSTREAM_DIR="${HOME}/Documents/github.com/google/ax"
  fi
fi

SUBSTRATE_UPSTREAM_DIR="${SUBSTRATE_UPSTREAM_DIR:-}"
if [[ -z "${SUBSTRATE_UPSTREAM_DIR}" ]]; then
  if [[ -d "${REPO_ROOT}/../agent-substrate/substrate" ]]; then
    SUBSTRATE_UPSTREAM_DIR="$(cd "${REPO_ROOT}/../agent-substrate/substrate" && pwd)"
  elif [[ -d "${HOME}/Documents/github.com/agent-substrate/substrate" ]]; then
    SUBSTRATE_UPSTREAM_DIR="${HOME}/Documents/github.com/agent-substrate/substrate"
  fi
fi

STAGE_DIR="$(mktemp -d -t ax-server-build-XXXXXX)"
trap 'rm -rf "${STAGE_DIR}"' EXIT

if [[ -n "${AX_UPSTREAM_DIR}" && -d "${AX_UPSTREAM_DIR}" ]]; then
  cp -a "${AX_UPSTREAM_DIR}/." "${STAGE_DIR}/"
  rm -rf "${STAGE_DIR}/.git"
else
  git clone --depth 1 https://github.com/google/ax.git "${STAGE_DIR}"
  rm -rf "${STAGE_DIR}/.git"
fi

(
  cd "${STAGE_DIR}"
  patch -p1 < "${PATCH_FILE}"
  if [[ -n "${SUBSTRATE_UPSTREAM_DIR}" && -d "${SUBSTRATE_UPSTREAM_DIR}" ]]; then
    go mod edit -replace "go.ate.dev/substrate=${SUBSTRATE_UPSTREAM_DIR}"
    go mod tidy
  fi
  mkdir -p "$(dirname "${OUT_BIN}")"
  CGO_ENABLED=0 GOOS="${TARGET_GOOS:-linux}" GOARCH="${TARGET_GOARCH:-amd64}" \
    go build -o "${OUT_BIN}" ./cmd/ax-server
)

echo "Built patched ax-server binary at ${OUT_BIN}"
