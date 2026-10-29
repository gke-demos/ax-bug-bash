#!/usr/bin/env bash
set -euo pipefail

export PATH="/usr/local/google/home/mofi/.local/go/bin:$HOME/.cargo/bin:$PATH"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${1:---all}"

run_service() {
  local svc="$1"
  case "$svc" in
    storefront-web)
      echo "==> [storefront-web] Running Node.js/TypeScript unit tests..."
      (cd "$SCRIPT_DIR/storefront-web" && npm test)
      ;;
    checkout-service)
      echo "==> [checkout-service] Running Go unit tests..."
      (cd "$SCRIPT_DIR/checkout-service" && go test -v ./...)
      ;;
    shipping-service)
      echo "==> [shipping-service] Running Go unit tests..."
      (cd "$SCRIPT_DIR/shipping-service" && go test -v ./...)
      ;;
    pricing-service)
      echo "==> [pricing-service] Running Python unit tests..."
      (cd "$SCRIPT_DIR/pricing-service" && PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s . -p "test_*.py" -v)
      ;;
    inventory-service)
      echo "==> [inventory-service] Running Rust unit tests..."
      if command -v cargo >/dev/null 2>&1; then
        (cd "$SCRIPT_DIR/inventory-service" && CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-/tmp/ax-cargo-target}" cargo test)
      else
        (cd "$SCRIPT_DIR/inventory-service" && python3 ./verify_rust_tests.py)
      fi
      ;;
    *)
      echo "Unknown service: $svc" >&2
      exit 2
      ;;
  esac
}

if [[ "$TARGET" == "--all" ]]; then
  for s in storefront-web checkout-service shipping-service pricing-service inventory-service; do
    run_service "$s"
  done
else
  run_service "$TARGET"
fi
