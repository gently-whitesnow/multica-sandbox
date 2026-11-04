// Package metrics exports controller quality metrics in the Prometheus text format
// (ADR 0025). Names use OpenTelemetry notation; the exporter adds unit and _total
// suffixes. Go runtime and process collectors keep their Prometheus names. Labels take
// only bounded values: never task, workspace, session or agent IDs, credentials,
// prompts or agent output. A nil *Metrics records nothing.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Path is the scrape endpoint, served on its own listener.
const Path = "/metrics"

// ReadyPath answers 200 once the controller serves at least one workspace, else 503.
// The host deploy gate polls it after a controller restart.
const ReadyPath = "/readyz"

// Version is the controller release from VERSION and Revision its source commit, both set
// at build time with -ldflags -X.
var Version, Revision = "unknown", "unknown"

// CardinalityLimit caps series per instrument; excess combinations go to the
// otel_metric_overflow series.
const CardinalityLimit = 2000

var (
	// attemptBuckets are the shared job.execution.duration defaults.
	attemptBuckets = []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 900, 1800, 3600, 7200, 14400}
	prepareBuckets = []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600}
	relayBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	// clientBuckets are the shared http.client.request.duration defaults.
	clientBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
)

// Events are the attempt lifecycle events counted outside the duration histogram;
// anything else counts as other.
var Events = []string{"start-rejected", "message-deferred", "report-deferred", "cancelled", "gone", "settled"}

var methods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions}

type Metrics struct {
	handler                         http.Handler
	attempt, prepare, relay, client metric.Float64Histogram
	active                          metric.Int64UpDownCounter
	events, retries                 metric.Int64Counter
	removed, pruned                 metric.Int64Counter
	pool                            atomic.Pointer[PoolReading]
	paused                          atomic.Pointer[string]
	draining, workspaces            atomic.Int64
}

// PoolReading is the attempt pool state of the last admission check.
type PoolReading struct {
	// Pressure is PSI "some" avg10 as a ratio; Usage the working set and Limit memory.high in bytes.
	Pressure     float64
	Usage, Limit int64
}

func New() (*Metrics, error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry), otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo(),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes))
	if err != nil {
		return nil, err
	}
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter), sdkmetric.WithCardinalityLimit(CardinalityLimit)).Meter("multica-sandbox")
	m := &Metrics{handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{})}
	var errs []error
	histogram := func(name, description string, buckets []float64) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(description), metric.WithExplicitBucketBoundaries(buckets...))
		errs = append(errs, err)
		return h
	}
	m.attempt = histogram("sandbox.attempt.duration", "Attempt time from claim to terminal report, by outcome.", attemptBuckets)
	m.prepare = histogram("sandbox.attempt.prepare.duration", "Attempt time from claim to agent start, by session continuity.", prepareBuckets)
	m.relay = histogram("sandbox.relay.request.duration", "Relay request time to response headers, by upstream status or refusal.", relayBuckets)
	m.client = histogram("http.client.request.duration", "Controller requests to Multica.", clientBuckets)
	m.active, err = meter.Int64UpDownCounter("sandbox.attempt.active", metric.WithUnit("{attempt}"), metric.WithDescription("Attempts in progress."))
	errs = append(errs, err)
	m.events, err = meter.Int64Counter("sandbox.attempt.events", metric.WithUnit("{event}"), metric.WithDescription("Attempt lifecycle events outside the duration histogram."))
	errs = append(errs, err)
	m.retries, err = meter.Int64Counter("http.client.request.retries", metric.WithUnit("{retry}"), metric.WithDescription("Resent controller requests to Multica."))
	errs = append(errs, err)
	m.removed, err = meter.Int64Counter("sandbox.sessions.removed", metric.WithUnit("{volume}"), metric.WithDescription("Removed session volumes, by trigger."))
	errs = append(errs, err)
	m.pruned, err = meter.Int64Counter("sandbox.sessions.pruned", metric.WithUnit("By"), metric.WithDescription("Rebuildable bytes pruned from idle session volumes, by trigger."))
	errs = append(errs, err)
	poolGauge := func(name, unit, description string, value func(PoolReading) float64) {
		_, err := meter.Float64ObservableGauge(name, metric.WithUnit(unit), metric.WithDescription(description),
			metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
				if r := m.pool.Load(); r != nil {
					o.Observe(value(*r))
				}
				return nil
			}))
		errs = append(errs, err)
	}
	poolGauge("sandbox.pool.memory.pressure", "1", "Attempt pool PSI some avg10.", func(r PoolReading) float64 { return r.Pressure })
	poolGauge("sandbox.pool.memory.usage", "By", "Attempt pool working set.", func(r PoolReading) float64 { return float64(r.Usage) })
	poolGauge("sandbox.pool.memory.limit", "By", "Attempt pool memory.high.", func(r PoolReading) float64 { return float64(r.Limit) })
	_, err = meter.Int64ObservableGauge("sandbox.claims.paused", metric.WithDescription("1 while claims wait for the attempt pool, by the last refusal reason, or for a controller restart."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if reason := m.paused.Load(); reason != nil {
				o.Observe(1, metric.WithAttributes(attribute.String("reason", *reason)))
			} else {
				o.Observe(0)
			}
			return nil
		}))
	errs = append(errs, err)
	_, err = meter.Int64ObservableGauge("sandbox.claims.draining", metric.WithDescription("1 while a deploy drain holds new claims."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(m.draining.Load())
			return nil
		}))
	errs = append(errs, err)
	_, err = meter.Int64ObservableGauge("sandbox.workspaces.ready", metric.WithUnit("{workspace}"), metric.WithDescription("Registered and recovered workspaces."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(m.workspaces.Load())
			return nil
		}))
	errs = append(errs, err)
	_, err = meter.Int64ObservableGauge("sandbox.build.info", metric.WithDescription("Always 1; the controller release version and source revision."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, metric.WithAttributes(attribute.String("version", Version), attribute.String("revision", Revision)))
			return nil
		}))
	errs = append(errs, err)
	if err = errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// Serve exposes Path on listen until ctx ends.
func (m *Metrics) Serve(ctx context.Context, listen string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+Path, m.handler)
	mux.HandleFunc("GET "+ReadyPath, m.ready)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = srv.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	return nil
}

func (m *Metrics) Handler() http.Handler { return m.handler }

func (m *Metrics) ready(w http.ResponseWriter, _ *http.Request) {
	if m.workspaces.Load() < 1 {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ready\n")
}

// Attempt records a reported attempt; reason is empty on success and code is the
// inference HTTP status of an inference failure, else 0.
func (m *Metrics) Attempt(kind, status, reason string, code int, total time.Duration) {
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("kind", kind), attribute.String("status", status)}
	if reason != "" {
		attrs = append(attrs, attribute.String("reason", reason))
	}
	if code != 0 {
		attrs = append(attrs, attribute.String("status_code", statusCode(code)))
	}
	m.attempt.Record(context.Background(), total.Seconds(), metric.WithAttributes(attrs...))
}

// Prepare records the time from claim to agent start.
func (m *Metrics) Prepare(kind, continuity string, d time.Duration) {
	if m == nil {
		return
	}
	m.prepare.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("kind", kind), attribute.String("continuity", continuity)))
}

func (m *Metrics) Active(delta int64) {
	if m == nil {
		return
	}
	m.active.Add(context.Background(), delta)
}

func (m *Metrics) Event(event string) {
	if m == nil {
		return
	}
	if !slices.Contains(Events, event) {
		event = "other"
	}
	m.events.Add(context.Background(), 1, metric.WithAttributes(attribute.String("event", event)))
}

// Relay records a relay request: the upstream status, or with errorType the refusal
// the relay answered itself.
func (m *Metrics) Relay(relay string, code int, errorType string, d time.Duration) {
	if m == nil {
		return
	}
	m.relay.Record(context.Background(), d.Seconds(), metric.WithAttributes(outcome([]attribute.KeyValue{attribute.String("relay", relay)}, code, errorType)...))
}

// Client records a request to target; path is its route template.
func (m *Metrics) Client(target, method, path string, code int, errorType string, d time.Duration) {
	if m == nil {
		return
	}
	attrs := outcome([]attribute.KeyValue{attribute.String("method", normalMethod(method)), attribute.String("path", path), attribute.String("target_service_name", target)}, code, errorType)
	m.client.Record(context.Background(), d.Seconds(), metric.WithAttributes(attrs...))
}

// Retry counts a resent request.
func (m *Metrics) Retry(target, method, path string) {
	if m == nil {
		return
	}
	m.retries.Add(context.Background(), 1, metric.WithAttributes(attribute.String("method", normalMethod(method)), attribute.String("path", path), attribute.String("target_service_name", target)))
}

// SessionTriggers are the session collection triggers; anything else counts as other.
var SessionTriggers = []string{"lifetime", "age", "budget", "pressure"}

// Sessions records one session volume collection.
func (m *Metrics) Sessions(trigger string, removed int, pruned int64) {
	if m == nil {
		return
	}
	if !slices.Contains(SessionTriggers, trigger) {
		trigger = "other"
	}
	attrs := metric.WithAttributes(attribute.String("trigger", trigger))
	m.removed.Add(context.Background(), int64(removed), attrs)
	m.pruned.Add(context.Background(), pruned, attrs)
}

func (m *Metrics) Pool(r PoolReading) {
	if m == nil {
		return
	}
	m.pool.Store(&r)
}

// Paused records whether claims wait for the attempt pool or a restart; reason is the
// bounded refusal reason (pool.Refusal*) or restart and is ignored when not paused.
func (m *Metrics) Paused(paused bool, reason string) {
	if m == nil {
		return
	}
	if !paused {
		m.paused.Store(nil)
		return
	}
	m.paused.Store(&reason)
}

func (m *Metrics) Draining(draining bool) {
	if m == nil {
		return
	}
	var v int64
	if draining {
		v = 1
	}
	m.draining.Store(v)
}

// Workspaces records how many workspaces are ready; ReadyPath needs at least one.
func (m *Metrics) Workspaces(n int) {
	if m == nil {
		return
	}
	m.workspaces.Store(int64(n))
}

// outcome adds status_code for a response, else error_type: a series carries exactly one.
func outcome(attrs []attribute.KeyValue, code int, errorType string) []attribute.KeyValue {
	if errorType != "" {
		return append(attrs, attribute.String("error_type", errorType))
	}
	return append(attrs, attribute.String("status_code", statusCode(code)))
}

func statusCode(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return strconv.Itoa(code)
}

func normalMethod(method string) string {
	if slices.Contains(methods, method) {
		return method
	}
	return "other"
}
