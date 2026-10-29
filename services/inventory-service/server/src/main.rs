use axum::{
    body::Body,
    extract::State,
    http::{HeaderMap, HeaderValue, Method, Request, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    backtrace::Backtrace,
    cell::RefCell,
    env,
    io::{Read, Write},
    net::{SocketAddr, TcpStream, ToSocketAddrs},
    panic::{self, AssertUnwindSafe},
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

const SERVICE_NAME: &str = "inventory-service";
const LANGUAGE_LABEL: &str = "Rust / Axum";
const SOURCE_FILE: &str = "src/lib.rs";

thread_local! {
    static LAST_PANIC_TRACE: RefCell<String> = RefCell::new(String::new());
}

#[derive(Clone)]
struct AppState {
    loki_push_url: String,
}

#[derive(Debug, Clone, Deserialize)]
struct ReserveItem {
    #[serde(default)]
    sku: String,
    #[serde(default)]
    available: u32,
    #[serde(default)]
    requested: u32,
    #[serde(default)]
    bonus_bundle_units: u32,
    #[serde(default)]
    weight_grams: u32,
    #[serde(default)]
    hazmat_class: String,
}

#[derive(Debug, Deserialize)]
struct ReserveRequest {
    #[serde(default)]
    items: Vec<ReserveItem>,
    #[serde(default)]
    available: u32,
    #[serde(default)]
    requested: u32,
    #[serde(default)]
    bonus_bundle_units: u32,
    #[serde(default)]
    weight_grams: u32,
    #[serde(default)]
    hazmat_class: String,
}

#[tokio::main]
async fn main() {
    if env::var("RUST_BACKTRACE").is_err() {
        env::set_var("RUST_BACKTRACE", "1");
    }

    panic::set_hook(Box::new(|info| {
        let bt = Backtrace::force_capture();
        let formatted = format!("{info}\nstack backtrace:\n{bt}");
        LAST_PANIC_TRACE.with(|cell| {
            *cell.borrow_mut() = formatted;
        });
    }));

    let addr_str = env::var("LISTEN_ADDR")
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| {
            let port = env::var("PORT")
                .ok()
                .filter(|s| !s.trim().is_empty())
                .unwrap_or_else(|| "8084".to_string());
            format!("127.0.0.1:{port}")
        });
    let listen_addr: SocketAddr = addr_str
        .parse()
        .expect("invalid LISTEN_ADDR for inventory-http-server");

    let state = Arc::new(AppState {
        loki_push_url: env::var("LOKI_PUSH_URL").unwrap_or_default().trim().to_string(),
    });

    // TODO(security): Internal cluster microservice behind polyglot-supervisor; network policy isolates direct pod traffic.
    let app = Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        .route("/api/inventory/reserve", post(reserve_handler))
        .layer(middleware::from_fn(security_middleware))
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(listen_addr)
        .await
        .expect("failed to bind inventory-http-server");
    axum::serve(listener, app)
        .await
        .expect("inventory-http-server error");
}

async fn security_middleware(req: Request<Body>, next: Next) -> Response {
    let allowed = matches!(
        *req.method(),
        Method::GET | Method::POST | Method::HEAD | Method::OPTIONS
    );
    let mut resp = if !allowed {
        (
            StatusCode::METHOD_NOT_ALLOWED,
            Json(json!({"error": "Method Not Allowed"})),
        )
            .into_response()
    } else {
        next.run(req).await
    };

    let headers = resp.headers_mut();
    headers.insert(
        "X-Content-Type-Options",
        HeaderValue::from_static("nosniff"),
    );
    headers.insert("X-Frame-Options", HeaderValue::from_static("DENY"));
    headers.insert(
        "Permissions-Policy",
        HeaderValue::from_static("camera=(), microphone=(), geolocation=()"),
    );
    headers.insert(
        "Content-Security-Policy",
        HeaderValue::from_static("default-src 'self'; object-src 'none'; frame-ancestors 'none'"),
    );
    headers.insert("Cache-Control", HeaderValue::from_static("no-store"));
    resp
}

async fn healthz() -> Json<Value> {
    Json(json!({"status": "ok", "service": SERVICE_NAME}))
}

async fn readyz() -> Json<Value> {
    Json(json!({"status": "ready", "service": SERVICE_NAME}))
}

async fn reserve_handler(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ReserveRequest>,
) -> Response {
    let req_id = headers
        .get("X-Storefront-Request-ID")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim()
        .to_string();
    let scenario_hdr = headers
        .get("X-Incident-Scenario")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .trim()
        .to_string();

    let items = if !req.items.is_empty() {
        req.items
    } else {
        vec![ReserveItem {
            sku: String::new(),
            available: req.available,
            requested: req.requested,
            bonus_bundle_units: req.bonus_bundle_units,
            weight_grams: req.weight_grams,
            hazmat_class: req.hazmat_class,
        }]
    };

    let mut reservations = Vec::with_capacity(items.len());

    for item in items {
        LAST_PANIC_TRACE.with(|cell| cell.borrow_mut().clear());
        let reserve_outcome = panic::catch_unwind(AssertUnwindSafe(|| {
            inventory_service::reserve_bundle_stock(
                item.available,
                item.requested,
                item.bonus_bundle_units,
            )
        }));

        let remaining = match reserve_outcome {
            Ok(Ok(rem)) => rem,
            Ok(Err(err_msg)) => {
                let bt = Backtrace::force_capture();
                let stack = format!(
                    "{err_msg} (services/inventory-service/src/lib.rs:reserve_bundle_stock)\nstack backtrace:\n{bt}"
                );
                return emit_failure_response(
                    &state.loki_push_url,
                    &req_id,
                    &scenario_hdr,
                    &err_msg,
                    &stack,
                );
            }
            Err(panic_payload) => {
                let panic_msg = if let Some(s) = panic_payload.downcast_ref::<&str>() {
                    (*s).to_string()
                } else if let Some(s) = panic_payload.downcast_ref::<String>() {
                    s.clone()
                } else {
                    "HTTP 500: RustU32UnderflowPanic in reserve_bundle_stock".to_string()
                };
                let hook_trace = LAST_PANIC_TRACE.with(|cell| cell.borrow().clone());
                let stack = if hook_trace.is_empty() {
                    let bt = Backtrace::force_capture();
                    format!(
                        "thread 'tokio-runtime-worker' panicked at services/inventory-service/src/lib.rs:reserve_bundle_stock:\n{panic_msg}\nstack backtrace:\n{bt}"
                    )
                } else {
                    format!("{panic_msg}\n{hook_trace}")
                };
                return emit_failure_response(
                    &state.loki_push_url,
                    &req_id,
                    &scenario_hdr,
                    &panic_msg,
                    &stack,
                );
            }
        };

        let hazmat_outcome = panic::catch_unwind(AssertUnwindSafe(|| {
            inventory_service::classify_warehouse_hazmat(item.weight_grams, &item.hazmat_class)
        }));

        let handling_tier = match hazmat_outcome {
            Ok(Ok(tier)) => tier,
            Ok(Err(err_msg)) => {
                let bt = Backtrace::force_capture();
                let stack = format!(
                    "{err_msg} (services/inventory-service/src/lib.rs:classify_warehouse_hazmat)\nstack backtrace:\n{bt}"
                );
                return emit_failure_response(
                    &state.loki_push_url,
                    &req_id,
                    &scenario_hdr,
                    &err_msg,
                    &stack,
                );
            }
            Err(panic_payload) => {
                let panic_msg = if let Some(s) = panic_payload.downcast_ref::<&str>() {
                    (*s).to_string()
                } else if let Some(s) = panic_payload.downcast_ref::<String>() {
                    s.clone()
                } else {
                    "HTTP 500: UnhandledHazmatClassError panic".to_string()
                };
                let hook_trace = LAST_PANIC_TRACE.with(|cell| cell.borrow().clone());
                let stack = format!("{panic_msg}\n{hook_trace}");
                return emit_failure_response(
                    &state.loki_push_url,
                    &req_id,
                    &scenario_hdr,
                    &panic_msg,
                    &stack,
                );
            }
        };

        reservations.push(json!({
            "sku": item.sku,
            "remaining_stock": remaining,
            "handling_tier": handling_tier,
        }));
    }

    let mut resp = (
        StatusCode::OK,
        Json(json!({
            "service": SERVICE_NAME,
            "reservations": reservations,
            "request_id": req_id,
        })),
    )
        .into_response();
    if !req_id.is_empty() {
        if let Ok(hv) = HeaderValue::from_str(&req_id) {
            resp.headers_mut().insert("X-Storefront-Request-ID", hv);
        }
    }
    resp
}

fn classify_error(msg: &str) -> (&'static str, &'static str) {
    if msg.contains("RustU32UnderflowPanic") {
        ("RustU32UnderflowPanic", "INC-102")
    } else if msg.contains("UnhandledHazmatClassError") {
        ("UnhandledHazmatClassError", "INC-104")
    } else {
        ("InventoryDomainError", "")
    }
}

fn emit_failure_response(
    loki_url: &str,
    req_id: &str,
    scenario_hdr: &str,
    err_msg: &str,
    stack: &str,
) -> Response {
    let (err_type, inferred_inc) = classify_error(err_msg);
    let incident_id = if !scenario_hdr.is_empty() {
        scenario_hdr.to_string()
    } else {
        inferred_inc.to_string()
    };

    let now_dur = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let ts_nano = now_dur.as_nanos().to_string();
    let ts_secs = now_dur.as_secs();

    let log_entry = json!({
        "timestamp": format!("{ts_secs}"),
        "level": "ERROR",
        "request_id": req_id,
        "incident_id": incident_id,
        "service": SERVICE_NAME,
        "language": LANGUAGE_LABEL,
        "source_file": SOURCE_FILE,
        "error_type": err_type,
        "message": err_msg,
        "stack_trace": stack,
    });
    let log_line = log_entry.to_string();
    println!("{log_line}");

    if !loki_url.is_empty() {
        let url_owned = loki_url.to_string();
        let inc_owned = incident_id.clone();
        let line_owned = log_line.clone();
        let nano_owned = ts_nano.clone();
        tokio::task::spawn_blocking(move || {
            push_to_loki(&url_owned, &inc_owned, &nano_owned, &line_owned);
        });
    }

    let mut resp = (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(json!({
            "status": "FAILED",
            "service": SERVICE_NAME,
            "language": LANGUAGE_LABEL,
            "source_file": SOURCE_FILE,
            "incident_id": incident_id,
            "error_type": err_type,
            "message": err_msg,
            "stack_trace": stack,
            "request_id": req_id,
        })),
    )
        .into_response();
    if !req_id.is_empty() {
        if let Ok(hv) = HeaderValue::from_str(req_id) {
            resp.headers_mut().insert("X-Storefront-Request-ID", hv);
        }
    }
    resp
}

fn push_to_loki(loki_url: &str, incident_id: &str, ts_nano: &str, log_line: &str) {
    let stripped = match loki_url.strip_prefix("http://") {
        Some(s) => s,
        None => return,
    };
    let (host_port_raw, path_part) = match stripped.split_once('/') {
        Some((hp, p)) => (hp, format!("/{p}")),
        None => (stripped, "/".to_string()),
    };
    let host_port = if host_port_raw.contains(':') {
        host_port_raw.to_string()
    } else {
        format!("{host_port_raw}:80")
    };

    let mut stream_labels = serde_json::Map::new();
    stream_labels.insert("app".to_string(), json!("storefront"));
    stream_labels.insert("service".to_string(), json!(SERVICE_NAME));
    stream_labels.insert("language".to_string(), json!(LANGUAGE_LABEL));
    stream_labels.insert("level".to_string(), json!("ERROR"));
    if !incident_id.is_empty() {
        stream_labels.insert("incident_id".to_string(), json!(incident_id));
    }

    let payload = json!({
        "streams": [{
            "stream": stream_labels,
            "values": [[ts_nano, log_line]],
        }]
    })
    .to_string();

    let addrs = match host_port.to_socket_addrs() {
        Ok(a) => a,
        Err(_) => return,
    };
    for addr in addrs {
        if let Ok(mut stream) = TcpStream::connect_timeout(&addr, Duration::from_secs(2)) {
            let _ = stream.set_write_timeout(Some(Duration::from_secs(2)));
            let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
            let req = format!(
                "POST {path_part} HTTP/1.1\r\nHost: {host_port}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{payload}",
                payload.len()
            );
            if stream.write_all(req.as_bytes()).is_ok() {
                let mut sink = [0u8; 256];
                let _ = stream.read(&mut sink);
            }
            break;
        }
    }
}
