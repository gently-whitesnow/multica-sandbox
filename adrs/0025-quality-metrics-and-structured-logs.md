# ADR 0025: Quality metrics and structured logs

Status: Accepted
Date: 2026-10-30

## Context

Operators need outcomes, causes, latency, continuity and service health.
Plain-text logs made these hard to aggregate.

Deployment conventions for metrics and logs:

- metrics use the OpenTelemetry SDK and its Prometheus exporter; names come from
  OpenTelemetry semantic conventions where they define the measurement, and own
  names follow `<domain>.<object>.<measurement>`;
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
  `inference`, `agent` or `internal`. An `inference` failure adds the provider's HTTP `status_code`.
- `continuity`: `fresh`, `resumed`, or `lost` when a prior session could not be
  continued.
- Paused `reason`, the last refusal before the calm cooldown: `pressure`,
  `working_set`, `disk`, `headroom` or `unreadable`; `restart` while the controller
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
become labels. Both histograms are recorded when the attempt reports, because the
continuity of a run is known only then.

**Logs.** JSON lines on stdout carry `time`, `level`, `msg` and snake_case fields.
`attempt finished` carries
identity, `status`, `reason`, Multica's `failure_reason`, timings in seconds,
continuity, model, tokens, peak memory, session and the safe failure text Multica
also receives. Failures and degradations are `WARN`. `"debug": true` enables `DEBUG`
events: failed Multica requests (route template, status or `error_type`) and
the existing identity and inference diagnostics. Debug events never include headers,
query strings, bodies or credentials.

Relay logging distinguishes upstream responses (`err == nil`) from local refusals
and transport failures, regardless of HTTP status:

- Upstream 4xx except 429: `DEBUG`, `relay upstream response`. Protocol and discovery
  responses need not mean sandbox failure; neither endpoint exceptions nor attempt
  success determine this level. Investigate metrics and attempt outcomes together.
- Upstream 429/5xx: `WARN`, `relay upstream error`. Rate limiting signals reduced
  upstream capacity even if the caller later recovers.
- Local policy/credential refusals, capacity limits and transport failures: `WARN`,
  `relay request refused`, with `error_type`. Attempt failures remain `WARN`.
- Upstream responses below 400: metrics only.

Details carry relay, method, path, `route`, status or `error_type`, and a known
grant's `task` and `workspace`. Credential-like path segments become `{redacted}`;
`route` also turns identifiers into `{id}`. Queries, headers, bodies and credentials
are never logged. HTTP behavior, authorization and retries are unchanged.

Per relay, outcome and route, log the first event per minute; summarize subsequent
events as `relay events suppressed` with `count` and source `event`, at the original
level. Keep 256 detailed keys and three fixed overflow counters (route `*`), separate
for ordinary responses, degraded upstreams and local/transport errors. Overflow
summaries omit status/error type rather than attributing mixed outcomes to one code.
Disabled levels consume no sampling state or emit summaries; metrics count all
observations independently of logging and sampling. State and window swaps are locked.

## Consequences

Metrics expose outcomes, latency, retries and pressure; JSON logs retain details.
Consumers must match `msg` and fields. Scraping and shipping are deployment-owned.
Restart-interrupted attempts are not recorded. Model and token use stay in logs.
Dependencies: OpenTelemetry SDK/exporter and `prometheus/client_golang`.

## References

- [OpenTelemetry HTTP metrics semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/)
- [OpenTelemetry Prometheus exporter for Go](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/prometheus)
