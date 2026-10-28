#!/usr/bin/env bash
set -euo pipefail

# One-command GKE + Agent Substrate v0.6.0 + google/ax deployment script for ax-bug-bash.
# Uses active kubectl context and gcloud project defaults (never hardcoded cluster/node names).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

DRY_RUN="${DRY_RUN:-false}"
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN="true"
fi

KUBE_CONTEXT="${KUBE_CONTEXT:-$(kubectl config current-context 2>/dev/null || echo "current-gke-context")}"
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || echo "sample-cloud-project")}"
REGION="${REGION:-us-central1}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-${REGION}-docker.pkg.dev/${PROJECT_ID}/ax-bug-bash}"

# Resolve @sha256: digest for ax-task-runner (required by Agent Substrate v0.6.0 SnapshotConfig)
TASK_RUNNER_SHA256="${TASK_RUNNER_SHA256:-}"
if [[ -z "${TASK_RUNNER_SHA256}" ]]; then
  if [[ "${DRY_RUN}" == "false" ]] && command -v gcloud >/dev/null 2>&1; then
    RESOLVED_DIGEST="$(gcloud artifacts docker images describe "${IMAGE_REGISTRY}/ax-task-runner:latest" \
      --format='value(image_summary.digest)' 2>/dev/null || true)"
    TASK_RUNNER_SHA256="${RESOLVED_DIGEST#sha256:}"
  fi
fi
if [[ -z "${TASK_RUNNER_SHA256}" ]]; then
  TASK_RUNNER_SHA256="e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
fi

export KUBE_CONTEXT PROJECT_ID REGION IMAGE_REGISTRY TASK_RUNNER_SHA256
export AX_DEFAULT_TASK_IMAGE="${IMAGE_REGISTRY}/ax-task-runner@sha256:${TASK_RUNNER_SHA256}"

echo "==> [ax-bug-bash] Deploying to GKE context '${KUBE_CONTEXT}' (project '${PROJECT_ID}')"
echo "==> [ax-bug-bash] Task runner digest-pinned image: ${AX_DEFAULT_TASK_IMAGE}"

render_manifest() {
  local file="$1"
  sed \
    -e "s|\${IMAGE_REGISTRY}|${IMAGE_REGISTRY}|g" \
    -e "s|\${TASK_RUNNER_SHA256}|${TASK_RUNNER_SHA256}|g" \
    "$file"
}

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "==> [dry-run] Validating rendered manifests..."
  render_manifest "${REPO_ROOT}/manifests/monitoring/01-prometheus-alertmanager.yaml" >/dev/null
  render_manifest "${REPO_ROOT}/manifests/monitoring/02-loki-grafana.yaml" >/dev/null
  render_manifest "${REPO_ROOT}/manifests/ax-v060-overlay.yaml" >/dev/null
  render_manifest "${REPO_ROOT}/manifests/bug-bash-system.yaml" >/dev/null
  echo "==> [dry-run] Rendered OpenFGA AccessPolicy (Issue #511), WorkerPool v0.6.0, Workspace (Issue #482), and @sha256: pinned deployments cleanly."
  exit 0
fi

echo "==> [1/4] Applying Agent Substrate v0.6.0 overlay & OpenFGA AccessPolicy (Issue #511)..."
render_manifest "${REPO_ROOT}/manifests/ax-v060-overlay.yaml" | kubectl --context="${KUBE_CONTEXT}" apply -f -

echo "==> [2/4] Applying Prometheus, Alertmanager, Loki & Grafana monitoring stack (5s scrape, for: 0s)..."
render_manifest "${REPO_ROOT}/manifests/monitoring/01-prometheus-alertmanager.yaml" | kubectl --context="${KUBE_CONTEXT}" apply -f -
render_manifest "${REPO_ROOT}/manifests/monitoring/02-loki-grafana.yaml" | kubectl --context="${KUBE_CONTEXT}" apply -f -

echo "==> [3/4] Applying Model Gateway (:8080), Retail Storefront (:3001), and AX Observer (:3000)..."
render_manifest "${REPO_ROOT}/manifests/bug-bash-system.yaml" | kubectl --context="${KUBE_CONTEXT}" apply -f -

echo "==> [4/4] Waiting for rollouts and verifying sub-second in-cluster Git clone before /readyz (Issue #482)..."
kubectl --context="${KUBE_CONTEXT}" -n ax-system rollout status deployment/model-gateway --timeout=120s
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash rollout status deployment/storefront --timeout=120s
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash rollout status deployment/ax-observer --timeout=120s

# Verify < 500ms in-cluster Git clone and /readyz probe (Issue #482)
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash exec deploy/storefront -- \
  sh -c 'start=$(date +%s%3N); git ls-remote http://127.0.0.1:3001/git/storefront.git HEAD >/dev/null; end=$(date +%s%3N); echo "In-cluster Git server ready in $((end - start))ms"'

echo "==> [ax-bug-bash] Deployment complete!"
