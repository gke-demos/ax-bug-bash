import http from "node:http";
import type { IncomingMessage, ServerResponse } from "node:http";
import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, URL } from "node:url";

import { CATALOG_PRODUCTS, convertToCurrencyMinorUnits } from "./server.ts";

const SERVICE_NAME = "storefront-web";
const LANGUAGE_LABEL = "Node.js / TypeScript";
const SOURCE_FILE = "src/server.ts";
const ALL_SERVICES: string[] = [
  "storefront-web",
  "checkout-service",
  "shipping-service",
  "pricing-service",
  "inventory-service",
];

const PRODUCT_DESCRIPTIONS: Record<string, string> = {
  "sku-cable-usbc": "Heavy-duty braided USB-C PD 3.1 cable ($16.00).",
  "sku-mechanical-kb-bundle":
    "Hot-swappable aluminum mechanical keyboard with Flash-Sale 2-for-1 bonus keycap/switch bundle ($145.00).",
  "sku-desk-mat-xl": "900x400mm stitched merino wool desk mat ($29.00).",
  "sku-power-station-240wh":
    "High-capacity LiFePO4 field battery pack (3,400g, UN3480-LITHIUM class, $199.00).",
};

export interface CartItemInput {
  sku: string;
  quantity: number;
}

export interface CheckoutPayload {
  items?: CartItemInput[];
  currency?: string;
  promo_codes?: string[];
  flash_bundle?: boolean;
}

export interface ServiceTraceDetail {
  service: string;
  language: string;
  source_file: string;
  error_type: string;
  error_message: string;
  stack_trace: string;
}

export interface LogEntryRecord {
  timestamp: string;
  level: string;
  request_id: string;
  incident_id: string;
  service: string;
  language: string;
  source_file: string;
  error_type: string;
  message: string;
  stack_trace: string;
}

export interface BackendCallResult {
  status: number;
  body: Record<string, any>;
}

const recentLogs: LogEntryRecord[] = [];
let orderSeq: number = 0;

const currentFileDir = path.dirname(fileURLToPath(import.meta.url));
const staticDir = path.resolve(currentFileDir, "..", "static");

function resolveGitHttpBaseUrl(): string {
  const explicit = (process.env.STOREFRONT_GIT_HTTP_URL || "").trim();
  if (explicit) {
    return explicit.replace(/\/+$/, "");
  }
  const gitCloneUrl = (process.env.STOREFRONT_GIT_URL || "").trim();
  if (gitCloneUrl) {
    const parsed = new URL(gitCloneUrl);
    return `${parsed.protocol}//${parsed.host}`;
  }
  return "http://storefront-git:8090";
}

function setSecurityHeaders(res: ServerResponse): void {
  res.setHeader("X-Content-Type-Options", "nosniff");
  res.setHeader("X-Frame-Options", "DENY");
  res.setHeader("Permissions-Policy", "camera=(), microphone=(), geolocation=()");
  res.setHeader(
    "Content-Security-Policy",
    "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'"
  );
}

function sendJson(
  res: ServerResponse,
  status: number,
  payload: Record<string, any>,
  extraHeaders?: Record<string, string>
): void {
  setSecurityHeaders(res);
  res.setHeader("Content-Type", "application/json; charset=utf-8");
  res.setHeader("Cache-Control", "no-store");
  if (extraHeaders) {
    for (const [k, v] of Object.entries(extraHeaders)) {
      if (v) {
        res.setHeader(k, v);
      }
    }
  }
  const raw = Buffer.from(JSON.stringify(payload), "utf8");
  res.setHeader("Content-Length", String(raw.length));
  res.writeHead(status);
  res.end(raw);
}

function pushLokiAsync(
  lokiUrl: string,
  incidentId: string,
  tsNano: string,
  logLine: string
): void {
  if (!lokiUrl) {
    return;
  }
  const labels: Record<string, string> = {
    app: "storefront",
    service: SERVICE_NAME,
    language: LANGUAGE_LABEL,
    level: "ERROR",
  };
  if (incidentId) {
    labels.incident_id = incidentId;
  }
  const body = JSON.stringify({
    streams: [{ stream: labels, values: [[tsNano, logLine]] }],
  });
  fetch(lokiUrl, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    signal: AbortSignal.timeout(2000),
  }).catch(() => {});
}

function recordErrorLog(entry: LogEntryRecord): void {
  recentLogs.push(entry);
  if (recentLogs.length > 200) {
    recentLogs.shift();
  }
}

async function readJsonBody(req: IncomingMessage): Promise<CheckoutPayload> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    total += buf.length;
    if (total > 1024 * 1024) {
      throw new Error("request body too large");
    }
    chunks.push(buf);
  }
  const raw = Buffer.concat(chunks).toString("utf8").trim();
  if (!raw) {
    return {};
  }
  return JSON.parse(raw);
}

async function postBackendJson(
  targetUrl: string,
  reqId: string,
  scenarioHdr: string,
  payload: Record<string, any>
): Promise<BackendCallResult> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Storefront-Request-ID": reqId,
  };
  if (scenarioHdr) {
    headers["X-Incident-Scenario"] = scenarioHdr;
  }
  try {
    const resp = await fetch(targetUrl, {
      method: "POST",
      headers,
      body: JSON.stringify(payload),
      signal: AbortSignal.timeout(8000),
    });
    const data = await resp.json().catch(() => ({}));
    return { status: resp.status, body: data || {} };
  } catch (err: any) {
    return {
      status: 502,
      body: {
        error_type: "UpstreamConnectionError",
        message: String(err?.message || err),
        stack_trace: String(err?.stack || err),
      },
    };
  }
}

function inferIncidentAndErrorCode(
  scenarioHdr: string,
  combinedText: string
): Record<string, string> {
  if (
    combinedText.includes("NegativeSubtotalError") ||
    combinedText.includes("CarrierTariffBoundsError")
  ) {
    return {
      incidentId: scenarioHdr || "INC-101",
      errorCode: "PROMO_NEGATIVE_SUBTOTAL",
      summary:
        "HTTP 500: Stacked promo codes SAVE20 + FLAT15 produced negative subtotal (-220 cents) across pricing-service and shipping-service",
    };
  }
  if (
    combinedText.includes("RustU32UnderflowPanic") ||
    combinedText.includes("FlashBundleUnderflowError")
  ) {
    return {
      incidentId: scenarioHdr || "INC-102",
      errorCode: "FLASH_BUNDLE_STOCK_UNDERFLOW",
      summary:
        "HTTP 500: Flash-sale 2-for-1 bonus allocation exceeded remaining stock (available=2, requested=2, bonus=2), panicking inventory-service and checkout-service",
    };
  }
  if (
    combinedText.includes("StorefrontCurrencyPrecisionError") ||
    combinedText.includes("CurrencyPrecisionMismatchError") ||
    combinedText.includes("PaymentCapturePrecisionError")
  ) {
    return {
      incidentId: scenarioHdr || "INC-103",
      errorCode: "ZERO_DECIMAL_CURRENCY_PRECISION",
      summary:
        "HTTP 500: ISO-4217 zero-decimal currency JPY fractional minor unit rejected across storefront-web, pricing-service, and checkout-service",
    };
  }
  if (
    combinedText.includes("UnhandledHazmatClassError") ||
    combinedText.includes("HazmatWeightTierOverflowError")
  ) {
    return {
      incidentId: scenarioHdr || "INC-104",
      errorCode: "LITHIUM_HAZMAT_WEIGHT_OVERFLOW",
      summary:
        "HTTP 500: UnhandledHazmatClassError in inventory-service (UN3480-LITHIUM, 3400g) & HazmatWeightTierOverflowError in shipping-service",
    };
  }
  return {
    incidentId: scenarioHdr || "INC-UNKNOWN",
    errorCode: "CHECKOUT_PIPELINE_ERROR",
    summary: "HTTP 500: Multi-service checkout validation failed",
  };
}

function proxyRawHttp(
  req: IncomingMessage,
  res: ServerResponse,
  upstreamBase: string
): void {
  const upstreamUrl = new URL(req.url || "/", upstreamBase);
  const forwardedHeaders: Record<string, any> = { ...req.headers };
  forwardedHeaders.host = upstreamUrl.host;

  const proxyReq = http.request(
    {
      protocol: upstreamUrl.protocol,
      hostname: upstreamUrl.hostname,
      port: upstreamUrl.port,
      method: req.method,
      path: upstreamUrl.pathname + upstreamUrl.search,
      headers: forwardedHeaders,
    },
    (proxyRes) => {
      setSecurityHeaders(res);
      for (const [k, v] of Object.entries(proxyRes.headers)) {
        if (v !== undefined && k.toLowerCase() !== "transfer-encoding") {
          res.setHeader(k, v);
        }
      }
      res.writeHead(proxyRes.statusCode || 502);
      proxyRes.pipe(res);
    }
  );

  proxyReq.on("error", (err) => {
    sendJson(res, 502, {
      error: `storefront-git upstream unreachable: ${err.message}`,
    });
  });

  req.pipe(proxyReq);
}

async function serveStaticAsset(
  res: ServerResponse,
  relPath: string
): Promise<void> {
  const safeRel = relPath.replace(/^\/+/, "");
  const resolved = path.resolve(staticDir, safeRel);
  if (!resolved.startsWith(staticDir + path.sep)) {
    sendJson(res, 403, { error: "forbidden path" });
    return;
  }
  try {
    const content = await readFile(resolved);
    setSecurityHeaders(res);
    if (resolved.endsWith(".html")) {
      res.setHeader("Content-Type", "text/html; charset=utf-8");
    } else if (resolved.endsWith(".js")) {
      res.setHeader("Content-Type", "application/javascript; charset=utf-8");
    } else if (resolved.endsWith(".css")) {
      res.setHeader("Content-Type", "text/css; charset=utf-8");
    } else {
      res.setHeader("Content-Type", "application/octet-stream");
    }
    res.setHeader("Content-Length", String(content.length));
    res.writeHead(200);
    res.end(content);
  } catch {
    sendJson(res, 404, { error: "asset not found" });
  }
}

async function handleCheckout(
  req: IncomingMessage,
  res: ServerResponse,
  pricingUrl: string,
  inventoryUrl: string,
  shippingUrl: string,
  checkoutUrl: string,
  lokiUrl: string
): Promise<void> {
  const rawReqId = req.headers["x-storefront-request-id"];
  const reqId =
    (Array.isArray(rawReqId) ? rawReqId[0] : rawReqId || "").trim() ||
    `req-${randomUUID()}`;
  const rawScen = req.headers["x-incident-scenario"];
  const scenarioHdr = (
    Array.isArray(rawScen) ? rawScen[0] : rawScen || ""
  ).trim();

  let body: CheckoutPayload;
  try {
    body = await readJsonBody(req);
  } catch {
    sendJson(
      res,
      400,
      { status: "INVALID_REQUEST", error: "invalid JSON payload" },
      { "X-Storefront-Request-ID": reqId }
    );
    return;
  }

  const currency = String(body.currency || "USD").trim().toUpperCase() || "USD";
  const promoCodes: string[] = Array.isArray(body.promo_codes)
    ? body.promo_codes.map((p) => String(p))
    : [];
  const flashBundle = Boolean(body.flash_bundle);
  const cartItems: CartItemInput[] = Array.isArray(body.items) ? body.items : [];

  const catalogBySku = new Map(CATALOG_PRODUCTS.map((p) => [p.sku, p]));
  let subtotalCents = 0;
  let weightGrams = 0;
  let hazmatClass = "NONE";
  const inventoryItems: Record<string, any>[] = [];
  const checkoutItems: Record<string, any>[] = [];

  for (const item of cartItems) {
    const prod = catalogBySku.get(item.sku);
    const qty = Math.max(1, Number(item.quantity || 1));
    if (!prod) {
      continue;
    }
    subtotalCents += prod.priceUsdCents * qty;
    weightGrams += prod.weightGrams * qty;
    if (prod.hazmatClass && prod.hazmatClass !== "NONE") {
      hazmatClass = prod.hazmatClass;
    }
    const bonus =
      flashBundle || prod.flashBundleBonusUnits > 0
        ? prod.flashBundleBonusUnits
        : 0;
    inventoryItems.push({
      sku: prod.sku,
      available: prod.availableStock,
      requested: qty,
      bonus_bundle_units: bonus,
      weight_grams: prod.weightGrams * qty,
      hazmat_class: prod.hazmatClass,
    });
    checkoutItems.push({
      sku: prod.sku,
      available_stock: prod.availableStock,
      requested_qty: qty,
      bonus_bundle_units: bonus,
    });
  }

  let storefrontSelfFailed = false;
  const failingServices: string[] = [];
  const stackTraces: string[] = [];
  const serviceTraces: Record<string, ServiceTraceDetail> = {};

  // Step 1: Local storefront-web currency conversion check
  try {
    convertToCurrencyMinorUnits(subtotalCents, currency);
  } catch (err: any) {
    storefrontSelfFailed = true;
    const errMsg = String(err?.message || err);
    const stack = String(err?.stack || errMsg);
    const errType = errMsg.includes("StorefrontCurrencyPrecisionError")
      ? "StorefrontCurrencyPrecisionError"
      : "StorefrontRuntimeError";
    const incId = scenarioHdr || (errType === "StorefrontCurrencyPrecisionError" ? "INC-103" : "");
    const nowIso = new Date().toISOString();
    const tsNano = `${BigInt(Date.now()) * 1000000n}`;
    const logEntry: LogEntryRecord = {
      timestamp: nowIso,
      level: "ERROR",
      request_id: reqId,
      incident_id: incId,
      service: SERVICE_NAME,
      language: LANGUAGE_LABEL,
      source_file: SOURCE_FILE,
      error_type: errType,
      message: errMsg,
      stack_trace: stack,
    };
    const logLine = JSON.stringify(logEntry);
    process.stdout.write(logLine + "\n");
    pushLokiAsync(lokiUrl, incId, tsNano, logLine);
    recordErrorLog(logEntry);

    failingServices.push(SERVICE_NAME);
    stackTraces.push(stack);
    serviceTraces[SERVICE_NAME] = {
      service: SERVICE_NAME,
      language: LANGUAGE_LABEL,
      source_file: SOURCE_FILE,
      error_type: errType,
      error_message: errMsg,
      stack_trace: stack,
    };
  }

  // Step 2: inventory-service reservation & hazmat ledger validation
  const invRes = await postBackendJson(
    `${inventoryUrl}/api/inventory/reserve`,
    reqId,
    scenarioHdr,
    { items: inventoryItems }
  );
  if (invRes.status >= 400) {
    const stack = String(invRes.body.stack_trace || invRes.body.message || "");
    const detail: ServiceTraceDetail = {
      service: "inventory-service",
      language: String(invRes.body.language || "Rust / Axum"),
      source_file: String(invRes.body.source_file || "src/lib.rs"),
      error_type: String(invRes.body.error_type || "InventoryError"),
      error_message: String(invRes.body.message || "inventory-service error"),
      stack_trace: stack,
    };
    failingServices.push("inventory-service");
    stackTraces.push(stack);
    serviceTraces["inventory-service"] = detail;
    recordErrorLog({
      timestamp: new Date().toISOString(),
      level: "ERROR",
      request_id: reqId,
      incident_id: String(invRes.body.incident_id || scenarioHdr),
      service: "inventory-service",
      language: detail.language,
      source_file: detail.source_file,
      error_type: detail.error_type,
      message: detail.error_message,
      stack_trace: stack,
    });
  }

  // Step 3: pricing-service stacked promo & currency quote
  const prcRes = await postBackendJson(
    `${pricingUrl}/api/pricing/quote`,
    reqId,
    scenarioHdr,
    {
      subtotal_cents: subtotalCents,
      promo_codes: promoCodes,
      currency,
    }
  );
  let discountedSubtotalCents = subtotalCents;
  if (prcRes.status >= 400) {
    const stack = String(prcRes.body.stack_trace || prcRes.body.message || "");
    const errType = String(prcRes.body.error_type || "PricingError");
    const detail: ServiceTraceDetail = {
      service: "pricing-service",
      language: String(prcRes.body.language || "Python / FastAPI"),
      source_file: String(prcRes.body.source_file || "pricing.py"),
      error_type: errType,
      error_message: String(prcRes.body.message || "pricing-service error"),
      stack_trace: stack,
    };
    failingServices.push("pricing-service");
    stackTraces.push(stack);
    serviceTraces["pricing-service"] = detail;
    recordErrorLog({
      timestamp: new Date().toISOString(),
      level: "ERROR",
      request_id: reqId,
      incident_id: String(prcRes.body.incident_id || scenarioHdr),
      service: "pricing-service",
      language: detail.language,
      source_file: detail.source_file,
      error_type: detail.error_type,
      message: detail.error_message,
      stack_trace: stack,
    });
    if (errType.includes("NegativeSubtotalError")) {
      discountedSubtotalCents = 0;
    }
  } else {
    discountedSubtotalCents = Number(
      prcRes.body.discounted_subtotal_cents ?? subtotalCents
    );
  }

  // Step 4: shipping-service carrier tariff quote
  const shpRes = await postBackendJson(
    `${shippingUrl}/api/shipping/quote`,
    reqId,
    scenarioHdr,
    {
      subtotal_cents: discountedSubtotalCents,
      weight_grams: weightGrams,
      hazmat_class: hazmatClass,
    }
  );
  let shippingCents = 799;
  if (shpRes.status >= 400) {
    const stack = String(shpRes.body.stack_trace || shpRes.body.message || "");
    const detail: ServiceTraceDetail = {
      service: "shipping-service",
      language: String(shpRes.body.language || "Go"),
      source_file: String(shpRes.body.source_file || "shipping.go"),
      error_type: String(shpRes.body.error_type || "ShippingError"),
      error_message: String(shpRes.body.message || "shipping-service error"),
      stack_trace: stack,
    };
    failingServices.push("shipping-service");
    stackTraces.push(stack);
    serviceTraces["shipping-service"] = detail;
    recordErrorLog({
      timestamp: new Date().toISOString(),
      level: "ERROR",
      request_id: reqId,
      incident_id: String(shpRes.body.incident_id || scenarioHdr),
      service: "shipping-service",
      language: detail.language,
      source_file: detail.source_file,
      error_type: detail.error_type,
      message: detail.error_message,
      stack_trace: stack,
    });
  } else {
    shippingCents = Number(shpRes.body.rate_cents ?? 799);
  }

  // Step 5: checkout-service flash bundle & currency payment capture
  const totalUsdCents = Math.max(0, discountedSubtotalCents) + shippingCents;
  let captureAmountMinor = totalUsdCents;
  if (currency === "JPY") {
    // Raw FX conversion with 2 decimal places so checkout-service validates/rounds zero-decimal JPY independently
    captureAmountMinor = Number(
      (((storefrontSelfFailed ? subtotalCents : totalUsdCents) / 100) * 154.35).toFixed(2)
    );
  } else if (currency === "EUR") {
    captureAmountMinor = Math.round(totalUsdCents * 0.92);
  }

  const chkRes = await postBackendJson(
    `${checkoutUrl}/api/checkout/capture`,
    reqId,
    scenarioHdr,
    {
      items: checkoutItems,
      amount_minor: captureAmountMinor,
      currency,
    }
  );
  if (chkRes.status >= 400) {
    const stack = String(chkRes.body.stack_trace || chkRes.body.message || "");
    const detail: ServiceTraceDetail = {
      service: "checkout-service",
      language: String(chkRes.body.language || "Go"),
      source_file: String(chkRes.body.source_file || "checkout.go"),
      error_type: String(chkRes.body.error_type || "CheckoutError"),
      error_message: String(chkRes.body.message || "checkout-service error"),
      stack_trace: stack,
    };
    failingServices.push("checkout-service");
    stackTraces.push(stack);
    serviceTraces["checkout-service"] = detail;
    recordErrorLog({
      timestamp: new Date().toISOString(),
      level: "ERROR",
      request_id: reqId,
      incident_id: String(chkRes.body.incident_id || scenarioHdr),
      service: "checkout-service",
      language: detail.language,
      source_file: detail.source_file,
      error_type: detail.error_type,
      message: detail.error_message,
      stack_trace: stack,
    });
  }

  if (failingServices.length > 0) {
    const combined = stackTraces.join("\n");
    const inferred = inferIncidentAndErrorCode(scenarioHdr, combined);
    const unaffected = ALL_SERVICES.filter(
      (svc) => !failingServices.includes(svc)
    );
    const extraHeaders: Record<string, string> = {
      "X-Storefront-Request-ID": reqId,
    };
    if (!storefrontSelfFailed) {
      extraHeaders["X-Storefront-Upstream-Error-Only"] = "true";
    }
    sendJson(
      res,
      500,
      {
        status: "FAILED",
        incident_id: inferred.incidentId,
        error_code: inferred.errorCode,
        error: inferred.summary,
        implicated_services: failingServices,
        unaffected_services: unaffected,
        stack_traces: stackTraces,
        service_traces: serviceTraces,
      },
      extraHeaders
    );
    return;
  }

  orderSeq += 1;
  const chargedMinor = Number(
    chkRes.body.captured_minor_units ??
      convertToCurrencyMinorUnits(totalUsdCents, currency)
  );
  sendJson(
    res,
    200,
    {
      order_id: `ORD-${1000 + orderSeq}`,
      status: "CONFIRMED",
      currency,
      charged_minor_units: chargedMinor,
      shipping_cents: shippingCents,
    },
    { "X-Storefront-Request-ID": reqId }
  );
}

function parseListenAddr(): Record<string, any> {
  const listenAddr = (process.env.LISTEN_ADDR || "").trim();
  if (listenAddr && listenAddr.includes(":")) {
    const idx = listenAddr.lastIndexOf(":");
    const host = listenAddr.slice(0, idx) || "127.0.0.1";
    const port = Number(listenAddr.slice(idx + 1) || "3001");
    return { host, port };
  }
  const port = Number((process.env.PORT || "3001").trim() || "3001");
  return { host: "127.0.0.1", port };
}

function startServer(): void {
  const { host, port } = parseListenAddr();
  const pricingUrl = (
    process.env.PRICING_SERVICE_URL || "http://pricing-service:8083"
  ).replace(/\/+$/, "");
  const inventoryUrl = (
    process.env.INVENTORY_SERVICE_URL || "http://inventory-service:8084"
  ).replace(/\/+$/, "");
  const shippingUrl = (
    process.env.SHIPPING_SERVICE_URL || "http://shipping-service:8082"
  ).replace(/\/+$/, "");
  const checkoutUrl = (
    process.env.CHECKOUT_SERVICE_URL || "http://checkout-service:8081"
  ).replace(/\/+$/, "");
  const gitHttpBase = resolveGitHttpBaseUrl();
  const lokiUrl = (process.env.LOKI_PUSH_URL || "").trim();

  // TODO(security): Public demo storefront gateway; end-user checkout runs unauthenticated with in-memory cart state by architecture.
  const server = http.createServer(async (req, res) => {
    const method = req.method || "GET";
    if (!["GET", "POST", "HEAD", "OPTIONS"].includes(method)) {
      sendJson(res, 405, { error: "Method Not Allowed" });
      return;
    }
    if (method === "OPTIONS") {
      sendJson(res, 200, { status: "ok" });
      return;
    }

    const reqUrl = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
    const pathname = reqUrl.pathname;

    if (pathname === "/healthz") {
      sendJson(res, 200, { status: "ok", service: SERVICE_NAME });
      return;
    }
    if (pathname === "/readyz") {
      sendJson(res, 200, { status: "ready", service: SERVICE_NAME });
      return;
    }
    if (pathname === "/") {
      await serveStaticAsset(res, "index.html");
      return;
    }
    if (pathname.startsWith("/static/")) {
      await serveStaticAsset(res, pathname.slice("/static/".length));
      return;
    }
    if (pathname === "/api/products") {
      if (method !== "GET" && method !== "HEAD") {
        sendJson(res, 405, { error: "Method Not Allowed" });
        return;
      }
      const products = CATALOG_PRODUCTS.map((p) => ({
        sku: p.sku,
        name: p.name,
        category: p.category,
        description: PRODUCT_DESCRIPTIONS[p.sku] || p.name,
        price_usd_cents: p.priceUsdCents,
        weight_grams: p.weightGrams,
        hazmat_class: p.hazmatClass,
        available_stock: p.availableStock,
        flash_bundle_bonus_units: p.flashBundleBonusUnits,
      }));
      sendJson(res, 200, { products, services: ALL_SERVICES });
      return;
    }
    if (pathname === "/api/logs") {
      if (method !== "GET" && method !== "HEAD") {
        sendJson(res, 405, { error: "Method Not Allowed" });
        return;
      }
      const incFilter = (reqUrl.searchParams.get("incident_id") || "").trim();
      const entries = incFilter
        ? recentLogs.filter((e) => e.incident_id === incFilter)
        : recentLogs;
      sendJson(res, 200, { entries });
      return;
    }
    if (
      pathname.startsWith("/git/") ||
      pathname === "/api/incidents" ||
      pathname.startsWith("/api/incidents/")
    ) {
      proxyRawHttp(req, res, gitHttpBase);
      return;
    }
    if (pathname === "/api/scenarios") {
      req.url = "/api/incidents";
      proxyRawHttp(req, res, gitHttpBase);
      return;
    }
    if (pathname === "/api/cart/checkout" || pathname === "/api/checkout") {
      if (method !== "POST") {
        sendJson(res, 405, { error: "Method Not Allowed" });
        return;
      }
      await handleCheckout(
        req,
        res,
        pricingUrl,
        inventoryUrl,
        shippingUrl,
        checkoutUrl,
        lokiUrl
      );
      return;
    }

    sendJson(res, 404, { error: "not found" });
  });

  server.listen(port, host);
  const shutdown = () => {
    server.close(() => process.exit(0));
  };
  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);
}

startServer();
