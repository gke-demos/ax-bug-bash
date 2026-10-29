#!/usr/bin/env python3
"""Standalone HTTP server for pricing-service (FastAPI + Uvicorn with stdlib fallback)."""

from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import sys
import threading
import time
import traceback
from typing import Any, Dict, List, Optional, Tuple
import urllib.request

try:
    from fastapi import FastAPI, Request
    from fastapi.responses import JSONResponse
    import uvicorn

    _HAS_FASTAPI = True
except ImportError:
    _HAS_FASTAPI = False

from pricing import apply_stacked_promos, convert_currency_minor_units

SERVICE_NAME = "pricing-service"
LANGUAGE_LABEL = "Python / FastAPI"
SOURCE_FILE = "pricing.py"


def _classify_error(msg: str) -> Tuple[str, str]:
    if "NegativeSubtotalError" in msg:
        return "NegativeSubtotalError", "INC-101"
    if "CurrencyPrecisionMismatchError" in msg:
        return "CurrencyPrecisionMismatchError", "INC-103"
    return "PricingValueError", ""


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


def evaluate_quote_request(
    body: Dict[str, Any],
    request_id: str,
    scenario_hdr: str,
    loki_url: str,
) -> Tuple[int, Dict[str, Any]]:
    subtotal_cents = int(body.get("subtotal_cents", 0))
    raw_promos = body.get("promo_codes") or []
    promo_codes: List[str] = [str(p) for p in raw_promos]
    currency = str(body.get("currency") or "USD").strip().upper() or "USD"

    try:
        discounted_cents = apply_stacked_promos(subtotal_cents, promo_codes)
        converted_minor = convert_currency_minor_units(discounted_cents, currency)
        return 200, {
            "service": SERVICE_NAME,
            "discounted_subtotal_cents": discounted_cents,
            "converted_minor_units": converted_minor,
            "currency": currency,
            "request_id": request_id,
        }
    except Exception as exc:
        stack = traceback.format_exc()
        err_msg = str(exc)
        err_type, inferred_inc = _classify_error(err_msg)
        incident_id = scenario_hdr or inferred_inc
        now = datetime.now(timezone.utc)
        ts_iso = now.isoformat().replace("+00:00", "Z")
        ts_nano = str(time.time_ns())
        log_entry = {
            "timestamp": ts_iso,
            "level": "ERROR",
            "request_id": request_id,
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
            "request_id": request_id,
        }


def _parse_listen_addr() -> Tuple[str, int]:
    addr = os.environ.get("LISTEN_ADDR", "").strip()
    if addr and ":" in addr:
        host, port_s = addr.rsplit(":", 1)
        return (host or "127.0.0.1"), int(port_s)
    port = int(os.environ.get("PORT", "8083").strip() or "8083")
    return "127.0.0.1", port


def _run_fastapi(host: str, port: int, loki_url: str) -> bool:
    if not _HAS_FASTAPI:
        return False

    app = FastAPI(title="Storefront Pricing Service", docs_url=None, redoc_url=None)

    @app.middleware("http")
    async def security_headers_middleware(request: Request, call_next):  # type: ignore[no-untyped-def]
        if request.method not in ("GET", "POST", "HEAD", "OPTIONS"):
            return JSONResponse(status_code=405, content={"error": "Method Not Allowed"})
        response = await call_next(request)
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["X-Frame-Options"] = "DENY"
        response.headers["Permissions-Policy"] = "camera=(), microphone=(), geolocation=()"
        response.headers["Content-Security-Policy"] = "default-src 'self'; object-src 'none'; frame-ancestors 'none'"
        response.headers["Cache-Control"] = "no-store"
        return response

    @app.get("/healthz")
    async def healthz() -> Dict[str, str]:
        return {"status": "ok", "service": SERVICE_NAME}

    @app.get("/readyz")
    async def readyz() -> Dict[str, str]:
        return {"status": "ready", "service": SERVICE_NAME}

    @app.post("/api/pricing/quote")
    async def pricing_quote(request: Request) -> JSONResponse:
        req_id = (request.headers.get("X-Storefront-Request-ID") or "").strip()
        scenario_hdr = (request.headers.get("X-Incident-Scenario") or "").strip()
        try:
            payload = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "invalid JSON request"})
        status, out = evaluate_quote_request(payload, req_id, scenario_hdr, loki_url)
        headers: Dict[str, str] = {}
        if req_id:
            headers["X-Storefront-Request-ID"] = req_id
        return JSONResponse(status_code=status, content=out, headers=headers)

    uvicorn.run(app, host=host, port=port, log_level="warning")
    return True


def _run_stdlib_http(host: str, port: int, loki_url: str) -> None:
    # TODO(security): Internal cluster microservice behind polyglot-supervisor; network policy isolates direct pod traffic.
    class PricingHandler(BaseHTTPRequestHandler):
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
            if self.path != "/api/pricing/quote":
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
            status, out = evaluate_quote_request(body, req_id, scenario_hdr, loki_url)
            self._write_json(status, out, req_id)

        def do_PUT(self) -> None:  # noqa: N802
            self._write_json(405, {"error": "Method Not Allowed"})

        def do_DELETE(self) -> None:  # noqa: N802
            self._write_json(405, {"error": "Method Not Allowed"})

    srv = ThreadingHTTPServer((host, port), PricingHandler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        srv.server_close()


def main() -> None:
    host, port = _parse_listen_addr()
    loki_url = os.environ.get("LOKI_PUSH_URL", "").strip()
    if not _run_fastapi(host, port, loki_url):
        _run_stdlib_http(host, port, loki_url)


if __name__ == "__main__":
    main()
