#!/usr/bin/env python3
"""Workstation loopback fallback HTTP server for inventory-service when cargo/rustc is absent."""

from __future__ import annotations

from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import re
import sys
import threading
import time
from typing import Any, Dict, List, Tuple
import urllib.request

from verify_rust_tests import (
    eval_classify_warehouse_hazmat,
    eval_reserve_bundle_stock,
    load_lib_source,
    split_impl_and_tests,
)

SERVICE_NAME = "inventory-service"
LANGUAGE_LABEL = "Rust / Axum"
SOURCE_FILE = "src/lib.rs"


def check_rust_source() -> int:
    try:
        src = load_lib_source()
    except Exception as exc:
        sys.stderr.write(f"error reading src/lib.rs: {exc}\n")
        return 1
    if src.count("{") != src.count("}") or src.count("(") != src.count(")"):
        sys.stderr.write("syntax error: unbalanced delimiters in src/lib.rs\n")
        return 1
    if not re.search(r"pub\s+fn\s+reserve_bundle_stock\s*\(", src):
        sys.stderr.write("compile error: missing pub fn reserve_bundle_stock\n")
        return 1
    if not re.search(r"pub\s+fn\s+classify_warehouse_hazmat\s*\(", src):
        sys.stderr.write("compile error: missing pub fn classify_warehouse_hazmat\n")
        return 1
    return 0


def _push_loki_async(loki_url: str, incident_id: str, ts_nano: str, log_line: str) -> None:
    if not loki_url:
        return

    def _worker() -> None:
        labels: Dict[str, str] = {
            "app": "storefront",
            "service": SERVICE_NAME,
            "language": LANGUAGE_LABEL,
            "level": "ERROR",
        }
        if incident_id:
            labels["incident_id"] = incident_id
        payload = json.dumps(
            {"streams": [{"stream": labels, "values": [[ts_nano, log_line]]}]}
        ).encode("utf-8")
        req = urllib.request.Request(
            loki_url,
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=2.0) as resp:
                resp.read()
        except Exception:
            pass

    threading.Thread(target=_worker, daemon=True).start()


def _emit_failure(
    err_msg: str,
    fn_name: str,
    req_id: str,
    scenario_hdr: str,
    loki_url: str,
) -> Tuple[int, Dict[str, Any]]:
    if "RustU32UnderflowPanic" in err_msg:
        err_type = "RustU32UnderflowPanic"
        inferred_inc = "INC-102"
    elif "UnhandledHazmatClassError" in err_msg:
        err_type = "UnhandledHazmatClassError"
        inferred_inc = "INC-104"
    else:
        err_type = "InventoryDomainError"
        inferred_inc = ""

    incident_id = scenario_hdr or inferred_inc
    stack = (
        f"thread 'tokio-runtime-worker' panicked at services/inventory-service/src/lib.rs:20:9:\n"
        f"{err_msg}\n"
        f"stack backtrace:\n"
        f"   0: std::panicking::begin_panic_handler\n"
        f"   1: inventory_service::{fn_name}\n"
        f"             at ./services/inventory-service/src/lib.rs:20:9\n"
        f"   2: inventory_http_server::reserve_handler\n"
        f"             at ./services/inventory-service/server/src/main.rs:145:13\n"
    )
    now = datetime.now(timezone.utc)
    ts_iso = now.isoformat().replace("+00:00", "Z")
    ts_nano = str(time.time_ns())
    log_entry = {
        "timestamp": ts_iso,
        "level": "ERROR",
        "request_id": req_id,
        "incident_id": incident_id,
        "service": SERVICE_NAME,
        "language": LANGUAGE_LABEL,
        "source_file": SOURCE_FILE,
        "error_type": err_type,
        "message": err_msg,
        "stack_trace": stack,
    }
    log_line = json.dumps(log_entry, separators=(",", ":"))
    sys.stdout.write(log_line + "\n")
    sys.stdout.flush()
    _push_loki_async(loki_url, incident_id, ts_nano, log_line)
    return 500, {
        "status": "FAILED",
        "service": SERVICE_NAME,
        "language": LANGUAGE_LABEL,
        "source_file": SOURCE_FILE,
        "incident_id": incident_id,
        "error_type": err_type,
        "message": err_msg,
        "stack_trace": stack,
        "request_id": req_id,
    }


def evaluate_reserve_request(
    impl_src: str,
    body: Dict[str, Any],
    req_id: str,
    scenario_hdr: str,
    loki_url: str,
) -> Tuple[int, Dict[str, Any]]:
    raw_items = body.get("items")
    if isinstance(raw_items, list) and len(raw_items) > 0:
        items: List[Dict[str, Any]] = raw_items
    else:
        items = [body]

    reservations = []
    for item in items:
        avail = int(item.get("available", 0))
        req_qty = int(item.get("requested", 0))
        bonus = int(item.get("bonus_bundle_units", 0))
        weight = int(item.get("weight_grams", 0))
        hazmat = str(item.get("hazmat_class") or "NONE")

        try:
            tag, val = eval_reserve_bundle_stock(impl_src, avail, req_qty, bonus)
        except Exception as exc:
            return _emit_failure(str(exc), "reserve_bundle_stock", req_id, scenario_hdr, loki_url)
        if tag != "Ok":
            return _emit_failure(str(val), "reserve_bundle_stock", req_id, scenario_hdr, loki_url)

        h_tag, h_val = eval_classify_warehouse_hazmat(impl_src, weight, hazmat)
        if h_tag != "Ok":
            return _emit_failure(
                str(h_val), "classify_warehouse_hazmat", req_id, scenario_hdr, loki_url
            )

        reservations.append(
            {
                "sku": str(item.get("sku") or ""),
                "remaining_stock": val,
                "handling_tier": h_val,
            }
        )

    return 200, {
        "service": SERVICE_NAME,
        "reservations": reservations,
        "request_id": req_id,
    }


def main() -> int:
    if "--check" in sys.argv[1:]:
        return check_rust_source()

    if check_rust_source() != 0:
        return 1
    impl_src, _ = split_impl_and_tests(load_lib_source())

    addr = os.environ.get("LISTEN_ADDR", "").strip()
    if addr and ":" in addr:
        host, port_s = addr.rsplit(":", 1)
        host = host or "127.0.0.1"
        port = int(port_s)
    else:
        host = "127.0.0.1"
        port = int(os.environ.get("PORT", "8084").strip() or "8084")
    loki_url = os.environ.get("LOKI_PUSH_URL", "").strip()

    # TODO(security): Internal cluster microservice behind polyglot-supervisor; network policy isolates direct pod traffic.
    class InventoryHandler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
            return

        def _write_json(self, status: int, payload: Dict[str, Any], req_id: str = "") -> None:
            raw = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
            self.send_header("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'")
            if req_id:
                self.send_header("X-Storefront-Request-ID", req_id)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(raw)

        def do_HEAD(self) -> None:  # noqa: N802
            self.do_GET()

        def do_OPTIONS(self) -> None:  # noqa: N802
            self._write_json(200, {"status": "ok"})

        def do_GET(self) -> None:  # noqa: N802
            if self.path == "/healthz":
                self._write_json(200, {"status": "ok", "service": SERVICE_NAME})
                return
            if self.path == "/readyz":
                self._write_json(200, {"status": "ready", "service": SERVICE_NAME})
                return
            self._write_json(404, {"error": "not found"})

        def do_POST(self) -> None:  # noqa: N802
            if self.path != "/api/inventory/reserve":
                self._write_json(404, {"error": "not found"})
                return
            req_id = (self.headers.get("X-Storefront-Request-ID") or "").strip()
            scenario_hdr = (self.headers.get("X-Incident-Scenario") or "").strip()
            length = min(int(self.headers.get("Content-Length") or "0"), 1 << 20)
            raw = self.rfile.read(length) if length > 0 else b"{}"
            try:
                body = json.loads(raw.decode("utf-8"))
            except Exception:
                self._write_json(400, {"error": "invalid JSON request"}, req_id)
                return
            status, out = evaluate_reserve_request(impl_src, body, req_id, scenario_hdr, loki_url)
            self._write_json(status, out, req_id)

        def do_PUT(self) -> None:  # noqa: N802
            self._write_json(405, {"error": "Method Not Allowed"})

        def do_DELETE(self) -> None:  # noqa: N802
            self._write_json(405, {"error": "Method Not Allowed"})

    srv = ThreadingHTTPServer((host, port), InventoryHandler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
