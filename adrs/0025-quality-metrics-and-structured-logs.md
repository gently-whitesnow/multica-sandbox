# ADR 0025: Quality metrics and structured logs

Status: Accepted
Date: 2026-10-30

## Context

Operators need outcomes, causes, latency, continuity and service health.
Plain-text logs made these hard to aggregate.

Deployment conventions for metrics and logs:

- metrics use OpenTelemetry's SDK/Prometheus exporter and semantic convention names,
  otherwise `<domain>.<object>.<measurement>`;
- a service without its own HTTP server serves metrics on a dedicated port; the
  scraper adds `service_name` and the instance label;
- labels carry only bounded values, a missing value is a fixed fallback, and an
  inapplicable label is omitted; an SDK cardinality limit backs this up;
- next to a histogram no separate counter is added: counts come from `_count`;
- shared HTTP client metrics use `http.client.request.duration` and
  `http.client.request.retries` with `method`, `path` (route template), exactly one
  of `status_code` and `error_type`, and `target_service_name`.

## Decision

**Metrics.** `metrics_listen` serves only `/metrics` and `/readyz` (ADR 0031),
separately from agent-accessible relays. OpenTelemetry's Prometheus exporter omits
scope labels and `target_info`, limiting each instrument to 2000 series. Runtime
and process collectors retain standard `go_*` and `process_*` names:

| Metric (Prometheus) | Type | Labels | Question |
| --- | --- | --- | --- |
| `sandbox_attempt_duration_seconds` | histogram | `kind`, `status`, `reason`, `status_code` | Success and failure share and causes; attempt time from claim to report |
| `sandbox_attempt_prepare_duration_seconds` | histogram | `kind`, `continuity` | Time from claim to agent start; how often sessions resume |
| `sandbox_attempt_active` | gauge | — | Running attempts |
| `sandbox_attempt_events_total` | counter | `event` | Attempts cancelled, gone or settled elsewhere; start and report problems |
| `sandbox_relay_request_duration_seconds` | histogram | `relay`, `status_code` or `error_type` | Relay refusals and upstream answers, for example inference 403/422/429 |
| `http_client_request_duration_seconds` | histogram | shared contract, `target_service_name="multica"` | Multica API errors and latency |
| `http_client_request_retries_total` | counter | shared contract | Multica requests resent by the controller |
| `sandbox_pool_memory_pressure_ratio`, `sandbox_pool_memory_usage_bytes`, `sandbox_pool_memory_limit_bytes` | gauge | — | Attempt pool pressure at the last admission check |
| `sandbox_claims_paused` | gauge | `reason` while paused | Claims waiting for the pool or a restart, and why |
| `sandbox_claims_draining`, `sandbox_workspaces_ready` | gauge | — | Deploy drain and served workspaces (ADR 0031) |
| `sandbox_build_info` | gauge, always 1 | `version`, `revision` | Which controller release and commit run; deploys on a timeline |

Label values:

- `kind`: `issue`, `chat` or `other`.
- `status`: `success`, `failed` or `timeout` (the controller deadline).
- `reason`, failures only: `idle`, `memory_stall` (ADR 0032), `rejected`,
  `rejected_transient` (a host failure Multica retries, ADR 0006), `oom`,
  `host`, `session_unreadable`, `inference`, `agent` or `internal`. An `inference` failure adds the provider's HTTP `status_code`.
- `continuity`: `fresh`, `resumed`, or `lost` when a prior session could not be
  continued.
- Paused `reason`, the last refusal before the calm cooldown: `pressure`,
  `working_set`, `disk`, `inodes`, `headroom` or `unreadable`; `restart` while the controller
  waits for running attempts before exiting on a fatal attempt error (ADR 0006). The cold-start warmup paces
  starts and never pauses claims (ADR 0028).
- `version`: the release from `VERSION` (ADR 0033) and `revision` the source commit,
  both set at image build, else `unknown`.
- `event`: `start-rejected`, `message-deferred`, `report-deferred`, `cancelled`,
  `gone`, `settled` or `other`.
- `relay`: `multica`, `inference`, `git`, `forge` or `mcp`.
- Relay `error_type`: `policy-denied` (403, also a live grant's request outside its
  hosts, APIs or connections), `invalid-credential` (401: unknown or revoked only),
  `too-many-requests` (slots full; 429 with `Retry-After`), `upstream-credential`
  (502: unreadable host password or lapsed IAM token), `redirect-refused`,
  `body-limit`, or a transport error (`request-timeout`, `request-canceled`,
  `connection-failed`, `dns-resolve`, `tls-handshake`, `other`).
- Multica `path`: the request path with every segment outside a fixed word list
  replaced by `{id}`.

Task, workspace, agent and session IDs, models, credentials, prompts and output never
become labels. Attempt and preparation durations are recorded when the attempt reports.

Additional metrics record admission pause starts (`sandbox_claims_pauses_total`) and
completed pause duration (`sandbox_claims_pause_duration_seconds`) by initial reason,
including separate disk and inode causes. Warmup still paces starts independently.
`sandbox_attempt_start_delay_seconds` records queue creation to workload start by
kind, when Multica supplies a valid timestamp. `sandbox_attempt_failures_total`
counts reported failures by stage (`prepare` or `run`) and reason.

**Logs.** JSON stdout uses `time`, `level`, `msg` and snake_case fields.
Admission transitions include their reason, active attempts, memory readings when
available, and pause duration on resume. Attempt summaries include the dispatch
timestamp, outcome, failure stage and reason, duration, continuity, model, tokens,
peak memory and session. Retry starts link to the previous task when Multica supplies
it. Failed attempts include the safe failure text Multica also receives.

Successful relay responses emit metrics. Rate limits and upstream 5xx are warnings.
Local refusals and transport failures are warnings with `error_type`. Authorized MCP
401/403 and forge 404 emit info diagnostics with a route template. Other upstream
4xx are debug. Relay details identify the relay, method, route, duration, status or
error type and, for known grants, task, workspace and dispatch timestamp. MCP relay
logs carry the authorized connection name. Other paths redact credential-like
segments and replace identifiers in the sampling route. Headers, query strings and
bodies stay out of all events.

Each relay samples per outcome, route and configured connection: the first event per
minute is logged, later events are summarized at the same level. There are 256
detailed keys and one overflow bucket per event class. Overflow summaries omit
status and error type. Disabled levels keep no sampling state. Metrics count all
observations.

## Consequences

Metrics expose outcomes, latency, retries and pressure; JSON logs retain details.
Consumers must match `msg` and fields. Scraping and shipping are deployment-owned.
Restart-interrupted attempts are not recorded. Model and token use stay in logs.
Dependencies: OpenTelemetry SDK/exporter and `prometheus/client_golang`.

## References

- [OpenTelemetry HTTP metrics semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/)
- [OpenTelemetry Prometheus exporter for Go](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/prometheus)
