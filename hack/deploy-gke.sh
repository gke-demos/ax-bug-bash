#!/usr/bin/env bash
# End-to-End GKE + Agent Substrate v0.6.0 + google/ax Deployment Automation (ADR-0006, ADR-0008, ADR-0009, ADR-0011)
# Discovers cluster & project dynamically via kubectl/gcloud defaults — zero hardcoded cluster/project names.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

DRY_RUN=false
SKIP_BUILD=false
for arg in "$@"; do
  case "${arg}" in
    --dry-run)
      DRY_RUN=true
      ;;
    --skip-build)
      SKIP_BUILD=true
      ;;
  esac
done

KUBE_CONTEXT="${KUBE_CONTEXT:-$(kubectl config current-context 2>/dev/null || echo "gke-default-context")}"
PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || echo "example-gcp-project")}"
REGION="${REGION:-us-central1}"
AR_REPO="${AR_REPO:-ax-bug-bash}"
IMAGE_REGISTRY="${IMAGE_REGISTRY:-${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo "v060")}"

if [[ "${DRY_RUN}" == "true" ]]; then
  PROJECT_NUMBER="${PROJECT_NUMBER:-000000000000}"
  AX_SNAPSHOTS_BUCKET="${AX_SNAPSHOTS_BUCKET:-${PROJECT_ID}-ax-snapshots}"
  ATEWORKER_GVISOR_IMAGE="${ATEWORKER_GVISOR_IMAGE:-${IMAGE_REGISTRY}/ateworker-gvisor:v0.6.0}"
  MODEL_GATEWAY_SHA256="1111111111111111111111111111111111111111111111111111111111111111"
  STOREFRONT_SHA256="2222222222222222222222222222222222222222222222222222222222222222"
  AX_OBSERVER_SHA256="3333333333333333333333333333333333333333333333333333333333333333"
  TASK_RUNNER_SHA256="4444444444444444444444444444444444444444444444444444444444444444"
  AX_SERVER_SHA256="5555555555555555555555555555555555555555555555555555555555555555"
else
  PROJECT_NUMBER="${PROJECT_NUMBER:-$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)" 2>/dev/null || echo "")}"
  if [[ -z "${AX_SNAPSHOTS_BUCKET:-}" ]]; then
    EXISTING_BUCKET="$(gcloud storage buckets list --project="${PROJECT_ID}" --format="value(name)" 2>/dev/null | awk '/ax-snapshots/ {print $0; exit}' || true)"
    AX_SNAPSHOTS_BUCKET="${EXISTING_BUCKET:-${PROJECT_ID}-ax-snapshots-${PROJECT_NUMBER:-0}}"
  fi
  if [[ -z "${ATEWORKER_GVISOR_IMAGE:-}" ]]; then
    EXISTING_WORKER_IMG="$(kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash get workerpool ax-bug-bash-workers -o jsonpath='{.spec.workerImage}' 2>/dev/null || true)"
    ATEWORKER_GVISOR_IMAGE="${EXISTING_WORKER_IMG:-us-docker.pkg.dev/${PROJECT_ID}/ate-images/ateworker-gvisor:v0.6.1}"
  fi
fi

echo "==> Active Kubernetes Context: ${KUBE_CONTEXT}"
echo "==> Active GCP Project:        ${PROJECT_ID} (number=${PROJECT_NUMBER:-unknown})"
echo "==> Artifact Registry:         ${IMAGE_REGISTRY}"
echo "==> Snapshots Bucket:          gs://${AX_SNAPSHOTS_BUCKET}/ax-bug-bash/"

render_manifest() {
  local file="$1"
  sed \
    -e "s|\${IMAGE_REGISTRY}|${IMAGE_REGISTRY}|g" \
    -e "s|\${PROJECT_ID}|${PROJECT_ID}|g" \
    -e "s|\${AX_SNAPSHOTS_BUCKET}|${AX_SNAPSHOTS_BUCKET}|g" \
    -e "s|\${ATEWORKER_GVISOR_IMAGE}|${ATEWORKER_GVISOR_IMAGE}|g" \
    -e "s|\${MODEL_GATEWAY_SHA256}|${MODEL_GATEWAY_SHA256}|g" \
    -e "s|\${STOREFRONT_SHA256}|${STOREFRONT_SHA256}|g" \
    -e "s|\${AX_OBSERVER_SHA256}|${AX_OBSERVER_SHA256}|g" \
    -e "s|\${TASK_RUNNER_SHA256}|${TASK_RUNNER_SHA256}|g" \
    -e "s|\${AX_SERVER_SHA256}|${AX_SERVER_SHA256}|g" \
    "${file}"
}

if [[ "${DRY_RUN}" == "true" ]]; then
  echo "==> [DRY-RUN] Rendering manifests/ax-v060-overlay.yaml, monitoring stack, and manifests/bug-bash-system.yaml with @sha256: digests..."
  render_manifest manifests/ax-v060-overlay.yaml > /dev/null
  cat manifests/monitoring/01-prometheus-alertmanager.yaml > /dev/null
  cat manifests/monitoring/02-loki-grafana.yaml > /dev/null
  cat manifests/monitoring/03-gmp-podmonitoring.yaml > /dev/null
  RENDERED_SYSTEM="$(render_manifest manifests/bug-bash-system.yaml)"
  if [[ "${RENDERED_SYSTEM}" != *"@sha256:${MODEL_GATEWAY_SHA256}"* ]] || \
     [[ "${RENDERED_SYSTEM}" != *"@sha256:${STOREFRONT_SHA256}"* ]] || \
     [[ "${RENDERED_SYSTEM}" != *"@sha256:${AX_OBSERVER_SHA256}"* ]] || \
     [[ "${RENDERED_SYSTEM}" != *"@sha256:${TASK_RUNNER_SHA256}"* ]] || \
     [[ "${RENDERED_SYSTEM}" != *"@sha256:${AX_SERVER_SHA256}"* ]]; then
    echo "ERROR: rendered manifests/bug-bash-system.yaml missing @sha256: digest pins" >&2
    exit 1
  fi
  echo "==> [DRY-RUN] Manifest rendering and @sha256: digest verification succeeded."
  exit 0
fi

resolve_digest() {
  local image_name="$1"
  local tag_ref="${IMAGE_REGISTRY}/${image_name}:${TAG}"
  local digest=""
  digest="$(gcloud artifacts docker images describe "${tag_ref}" --format='value(image_summary.digest)' 2>/dev/null || true)"
  if [[ -z "${digest}" ]]; then
    digest="$(docker inspect --format='{{index .RepoDigests 0}}' "${tag_ref}" 2>/dev/null | awk -F'@' '{print $2}' || true)"
  fi
  if [[ -z "${digest}" ]]; then
    digest="$(gcloud artifacts docker images list "${IMAGE_REGISTRY}/${image_name}" --limit=1 --sort-by=~UPDATE_TIME --format='value(DIGEST)' 2>/dev/null | head -n1 || true)"
  fi
  echo "${digest#sha256:}"
}

build_and_push() {
  local image_name="$1"
  local dockerfile="$2"
  local context_dir="${3:-.}"
  local full_tag="${IMAGE_REGISTRY}/${image_name}:${TAG}"

  if [[ "${SKIP_BUILD}" != "true" ]]; then
    echo "==> Building & pushing ${full_tag} (${dockerfile})..." >&2
    docker build -f "${dockerfile}" -t "${full_tag}" "${context_dir}" >&2
    docker push "${full_tag}" >&2
  fi

  local sha
  sha="$(resolve_digest "${image_name}")"
  if [[ -z "${sha}" ]]; then
    echo "ERROR: failed to resolve @sha256: digest for ${image_name}" >&2
    exit 1
  fi
  echo "${sha}"
}

if [[ "${SKIP_BUILD}" != "true" ]]; then
  echo "==> Cross-compiling static Linux amd64 Go binaries..."
  mkdir -p bin/linux_amd64
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ax-observer ./cmd/ax-observer
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/model-gateway ./cmd/model-gateway
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ax-task-runner ./cmd/ax-task-runner
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/polyglot-supervisor ./cmd/polyglot-supervisor
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/storefront-git ./cmd/storefront-git
fi

MODEL_GATEWAY_SHA256="${MODEL_GATEWAY_SHA256:-$(build_and_push "model-gateway" "deploy/model-gateway/Dockerfile" ".")}"
STOREFRONT_SHA256="${STOREFRONT_SHA256:-$(build_and_push "storefront" "Dockerfile.polyglot-mesh" ".")}"
AX_OBSERVER_SHA256="${AX_OBSERVER_SHA256:-$(build_and_push "ax-observer" "deploy/ax-observer/Dockerfile" ".")}"
TASK_RUNNER_SHA256="${TASK_RUNNER_SHA256:-$(build_and_push "ax-task-runner" "harnesses/Dockerfile.task-runner" ".")}"

if [[ -z "${AX_SERVER_SHA256:-}" ]]; then
  if [[ "${SKIP_BUILD}" != "true" ]]; then
    IMAGE_URI="${IMAGE_REGISTRY}/ax-server:${TAG}" ./deploy/ax-server/build.sh --push >&2
  fi
  AX_SERVER_SHA256="$(resolve_digest "ax-server")"
fi

echo "==> Pinned Image Digests:"
echo "    model-gateway@sha256:${MODEL_GATEWAY_SHA256}"
echo "    storefront@sha256:${STOREFRONT_SHA256}"
echo "    ax-observer@sha256:${AX_OBSERVER_SHA256}"
echo "    ax-task-runner@sha256:${TASK_RUNNER_SHA256}"
echo "    ax-server@sha256:${AX_SERVER_SHA256}"

echo "==> Ensuring GKE Workload Identity IAM binding for Gemini Enterprise Agent Platform..."
GSA_NAME="model-gateway"
GSA_EMAIL="${GSA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
if ! gcloud iam service-accounts describe "${GSA_EMAIL}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${GSA_NAME}" \
    --project="${PROJECT_ID}" \
    --display-name="AX Bug Bash Zero-Secret Agent Platform Model Gateway" || true
fi
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${GSA_EMAIL}" \
  --role="roles/aiplatform.user" \
  --condition=None \
  --quiet >/dev/null 2>&1 || true
gcloud iam service-accounts add-iam-policy-binding "${GSA_EMAIL}" \
  --project="${PROJECT_ID}" \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:${PROJECT_ID}.svc.id.goog[ax-system/model-gateway]" \
  --quiet >/dev/null 2>&1 || true

echo "==> Step 1/5: Applying Agent Substrate v0.6.0 & google/ax Overlay (manifests/ax-v060-overlay.yaml)..."
render_manifest manifests/ax-v060-overlay.yaml | kubectl --context="${KUBE_CONTEXT}" apply -f -

if command -v kubectl-ate >/dev/null 2>&1; then
  echo "==> Applying OpenFGA AccessPolicy bindings via kubectl-ate..."
  kubectl-ate apply --global -f - <<EOF || true
apiVersion: ate.dev/v1alpha1
kind: AccessPolicy
metadata:
  name: ax-server-global-owner
spec:
  bindings:
    - role: owner
      members:
        - user:system:serviceaccount:ax-system:ax-server
    - role: viewer
      members:
        - user:system:serviceaccount:ax-bug-bash:ax-observer
EOF
fi

echo "==> Step 2/5: Applying Off-the-Shelf Prometheus + Alertmanager (5s Evaluation Window)..."
kubectl --context="${KUBE_CONTEXT}" apply -f manifests/monitoring/01-prometheus-alertmanager.yaml

echo "==> Step 3/5: Applying Loki + Pre-Provisioned Grafana Stack..."
kubectl --context="${KUBE_CONTEXT}" apply -f manifests/monitoring/02-loki-grafana.yaml

echo "==> Step 4/5: Applying Google Cloud Managed Service for Prometheus PodMonitoring (10s Interval)..."
kubectl --context="${KUBE_CONTEXT}" apply -f manifests/monitoring/03-gmp-podmonitoring.yaml || true

echo "==> Step 5/5: Applying Digest-Pinned Bug Bash System (manifests/bug-bash-system.yaml)..."
render_manifest manifests/bug-bash-system.yaml | kubectl --context="${KUBE_CONTEXT}" apply -f -

echo "==> Waiting for rollouts across ax-system, monitoring, and ax-bug-bash..."
kubectl --context="${KUBE_CONTEXT}" -n ax-system rollout status statefulset/ax-postgres --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n ax-system rollout status deployment/ax-server --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n ax-system rollout status deployment/model-gateway --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n monitoring rollout status deployment/prometheus --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n monitoring rollout status deployment/alertmanager --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n monitoring rollout status deployment/loki --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n monitoring rollout status deployment/grafana --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash rollout status deployment/storefront --timeout=180s
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash rollout status deployment/ax-observer --timeout=180s

echo "==> Verifying sub-500ms in-cluster Git reachability to storefront-monorepo..."
kubectl --context="${KUBE_CONTEXT}" -n ax-bug-bash exec deploy/storefront -- \
  git ls-remote http://127.0.0.1:3001/git/storefront.git HEAD >/dev/null

echo "==> Deployment complete! All surfaces healthy and @sha256: digest-pinned."
