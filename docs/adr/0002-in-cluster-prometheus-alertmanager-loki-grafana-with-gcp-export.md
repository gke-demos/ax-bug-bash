# In-Cluster Prometheus, Alertmanager, Loki, and Grafana with GCP Dual-Export

We deploy upstream Prometheus, Alertmanager, Loki, and Grafana in-cluster with a 5-second scrape and evaluation window (`for: 0s`) to fire real Alertmanager webhooks (`POST /api/webhooks/alertmanager`) within 3–5 seconds of any storefront HTTP 500 error, avoiding the 60–120 second evaluation delay of cloud-only alerting policies during live stage demos while simultaneously exporting telemetry to Google Cloud Logging and Google Cloud Managed Service for Prometheus.
