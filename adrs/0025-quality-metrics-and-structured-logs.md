# ADR 0025: Quality metrics and structured logs

Status: Accepted
Date: 2026-10-30

## Context

Operators judge the sandbox by outcomes: how many attempts succeed, why the others
fail, how long attempts and their preparation take, how often sessions resume, and
whether relays, Multica and the attempt pool hold up. The controller wrote plain-text
lines to stdout, one `finished` summary per attempt among them, and exposed no metrics.
Text lines cannot be aggregated, and a log shipper has to parse them.

The deployment scrapes metrics with Prometheus and ships container output to a log
store. Its conventions for services:

- metrics use the OpenTelemetry SDK and its Prometheus exporter; names come from
  OpenTelemetry semantic conventions where they define the measurement, and own
  names follow `<domain>.<object>.<measurement>`;
- a service without its own HTTP server serves `/metrics-text` on a dedicated port;
  the scraper adds `service_name` and the instance label;
- labels carry only bounded values, a missing value is a fixed fallback, and an
  inapplicable label is omitted; an SDK cardinality limit backs this up;
- next to a histogram no separate counter is added: counts come from `_count`;
- shared HTTP client metrics use `http.client.request.duration` and
  `http.client.request.retries` with `method`, `path` (route template), exactly one
  of `status_code` and `error_type`, and `target_service_name`.

## Decision

**Metrics.** With `metrics_listen` set, the controller serves `/metrics-text` on that
address and nothing else there. The listener is separate from the relays, which
attempts can reach. Instruments use the OpenTelemetry Go SDK and Prometheus exporter,
without scope labels and `target_info`, with a limit of 2000 series per instrument:

| Metric (Prometheus) | Type | Labels | Question |
| --- | --- | --- | --- |
| `sandbox_attempt_duration_seconds` | histogram | `kind`, `status`, `reason`, `status_code` | Success and failure share and causes; attempt time from claim to report |
| `sandbox_attempt_prepare_duration_seconds` | histogram | `kind`, `continuity` | Time from claim to agent start; how often sessions resume |
| `sandbox_attempt_active` | gauge | — | Busy slots |
| `sandbox_attempt_events_total` | counter | `event` | Attempts cancelled, gone or settled elsewhere; start and report problems |
| `sandbox_relay_request_duration_seconds` | histogram | `relay`, `status_code` or `error_type` | Relay refusals and upstream answers, for example inference 403/422/429 |
| `http_client_request_duration_seconds` | histogram | shared contract, `target_service_name="multica"` | Multica API errors and latency |
| `http_client_request_retries_total` | counter | shared contract | Multica requests resent by the controller |
| `sandbox_pool_memory_pressure_ratio`, `sandbox_pool_memory_usage_bytes`, `sandbox_pool_memory_limit_bytes` | gauge | — | Attempt pool pressure at the last admission check |
| `sandbox_claims_paused` | gauge | — | Claims waiting for the pool |

Label values:

- `kind`: `issue`, `chat` or `other`.
- `status`: `success`, `failed` or `timeout` (the controller deadline).
- `reason`, failures only: `idle`, `rejected`, `oom`, `inference`, `agent` or
  `internal`. An `inference` failure adds the provider's HTTP `status_code`.
- `continuity`: `fresh`, `resumed`, or `lost` when a prior session could not be
  continued.
- `event`: `start-rejected`, `message-deferred`, `report-deferred`, `cancelled`,
  `gone`, `settled` or `other`.
- `relay`: `multica`, `inference`, `git`, `forge` or `mcp`.
- Relay `error_type`: `policy-denied`, `invalid-credential`, `redirect-refused`,
  `body-limit`, or a transport error (`request-timeout`, `request-canceled`,
  `connection-failed`, `dns-resolve`, `tls-handshake`, `other`).
- Multica `path`: the request path with every segment outside a fixed word list
  replaced by `{id}`.

Task, workspace, agent and session IDs, models, credentials, prompts and output never
become labels. Both histograms are recorded when the attempt reports, because the
continuity of a run is known only then.

**Logs.** The controller writes JSON lines to stdout: `time`, `level`, `msg` and event
fields in snake_case. `attempt finished` replaces the `finished` line and carries
identity, `status`, `reason`, Multica's `failure_reason`, timings in seconds,
continuity, model, tokens, peak memory, session and the safe failure text Multica
also receives. Failures and degradations are `WARN`. `"debug": true` enables `DEBUG`
events: relay refusals and upstream errors (relay, method, path, status or
`error_type`), failed Multica requests (route template, status or `error_type`) and
the existing identity and inference diagnostics. Debug events never include headers,
query strings, bodies or credentials.

## Consequences

Success rate, failure causes, latency percentiles, retries, refusals and pool
pressure are queryable without parsing logs, and a log shipper reads the output as
JSON. Log consumers that matched the old text lines must match `msg` and fields
instead. The scrape port, its exposure and any log shipping are deployment
configuration. Attempts interrupted by a controller restart are not recorded. Model
and token use stay in logs only. This adds the OpenTelemetry metric SDK, its
Prometheus exporter and `prometheus/client_golang`.

## References

- [OpenTelemetry HTTP metrics semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/)
- [OpenTelemetry Prometheus exporter for Go](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/prometheus)
