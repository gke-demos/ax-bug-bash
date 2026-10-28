# Polyglot Five-Service Storefront, Incident Catalog, and Pre-Warmed Git Rollouts

We structure the target application as a 5-service polyglot retail monorepo (`storefront-web` in Node.js/TypeScript, `checkout-service` in Go, `shipping-service` in Go, `pricing-service` in Python/FastAPI, and `inventory-service` in Rust/Axum) backed by an in-cluster Git server and pre-warmed language build caches (`cargo`, `GOCACHE`, `venv`, `node_modules`) so both injecting any of the 4 multi-service Incident Scenarios (`INC-101`–`INC-104`) and rolling out verified agent fixes complete in under 10 seconds.
