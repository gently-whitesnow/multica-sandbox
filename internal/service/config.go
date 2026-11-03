package service

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
)

type Config struct {
	OpenCode  *OpenCodeConfig `json:"opencode,omitempty"`
	Server    string          `json:"server"`
	AllowHTTP bool            `json:"allow_http,omitempty"`
	Daemon    string          `json:"daemon"`
	Image     string          `json:"image"`
	Tools     []Tool          `json:"tools,omitempty"`
	Command   []string        `json:"command"`
	Timeout   string          `json:"timeout"`
	Debug     bool            `json:"debug,omitempty"`
	// MetricsListen, when set, serves metrics.Path on this host:port (ADR 0025).
	MetricsListen string      `json:"metrics_listen,omitempty"`
	Drain         <-chan bool `json:"-"`
	log           *slog.Logger
	metrics       *metrics.Metrics
}

func newLogger(out io.Writer, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}))
}

func (c Config) logger() *slog.Logger {
	if c.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.log
}

func (c Config) debugf() func(string, ...any) {
	if !c.Debug || c.log == nil {
		return nil
	}
	return func(format string, args ...any) { c.log.Debug(fmt.Sprintf(format, args...)) }
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func ReadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("open controller config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if info.Size() > 65536 {
		return c, fmt.Errorf("controller configuration exceeds size limit")
	}
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok && (field == `"concurrency"` || field == `"workspace_concurrency"`) {
			return c, fmt.Errorf("controller %s was removed: set max_concurrent_tasks on Multica agents", field)
		}
		return c, fmt.Errorf("invalid controller configuration")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("unexpected trailing configuration")
	}
	duration, err := time.ParseDuration(c.Timeout)
	if err != nil || duration <= 0 || !uuid.MatchString(c.Daemon) || c.Image == "" || (len(c.Command) == 0 && c.OpenCode == nil) {
		return c, fmt.Errorf("daemon UUID, image, command and positive timeout required")
	}
	if _, _, err := net.SplitHostPort(c.MetricsListen); c.MetricsListen != "" && err != nil {
		return c, fmt.Errorf("metrics_listen must be host:port")
	}
	if len(c.Tools) > 0 && c.OpenCode == nil {
		return c, fmt.Errorf("tools require the opencode adapter")
	}
	if c.OpenCode != nil {
		if err := c.OpenCode.validateCompaction(); err != nil {
			return c, err
		}
	}
	return c, nil
}

type OpenCodeConfig struct {
	IdentityFile string       `json:"identity_file,omitempty"`
	MCPRelay     *RelayConfig `json:"mcp_relay,omitempty"`
	// MCPPublicProxy enables arbitrary selected public MCP endpoints through trusted egress.
	MCPPublicProxy string         `json:"mcp_public_proxy,omitempty"`
	Authority      attempt.Config `json:"authority"`
	Network        string         `json:"network"`
	Peers          []string       `json:"peers"`
	MulticaRelay   *RelayConfig   `json:"multica_relay,omitempty"`
	// MulticaCLI is the digest-pinned CLI artifact the relay requires (deploy/multica-cli.Dockerfile).
	MulticaCLI string `json:"multica_cli,omitempty"`
	// MulticaCLIRevision is the commit the CLI artifact was built from (default multica.UpstreamRevision).
	MulticaCLIRevision string `json:"multica_cli_revision,omitempty"`
	// InferenceFile holds workspace gateway bindings; InferenceRelay is required with it.
	InferenceFile  string       `json:"inference_file,omitempty"`
	InferenceRelay *RelayConfig `json:"inference_relay,omitempty"`
	// Helper is the digest-pinned sandbox-helper image (Dockerfile target helper); with
	// the Multica relay it gives attempts a workdir volume and the checkout endpoint.
	Helper string `json:"helper,omitempty"`
	// GitFile holds workspace Git host bindings (repo.Config); GitRelay serves them (ADR 0015).
	GitFile  string       `json:"git_file,omitempty"`
	GitRelay *RelayConfig `json:"git_relay,omitempty"`
	// ForgeRelay serves gh for Git hosts with an api; the helper forwards loopback 443 to its url.
	ForgeRelay *RelayConfig `json:"forge_relay,omitempty"`
	// Sessions, with the helper, retains issue workdirs and resumes sessions (ADR 0015).
	Sessions     *SessionsConfig `json:"sessions,omitempty"`
	IdleWatchdog string          `json:"idle_watchdog,omitempty"`
	// CompactionMaxTokens is an optional native compaction trigger cap for verified OpenCode 2.x.
	CompactionMaxTokens int                  `json:"compaction_max_tokens,omitempty"`
	Resources           *docker.ResourceSpec `json:"resources,omitempty"`
	Runtime             string               `json:"runtime,omitempty"`
	DinD                *DinDConfig          `json:"dind,omitempty"`
	Pool                *PoolConfig          `json:"pool,omitempty"`
}

// SessionsConfig bounds retained workdirs: TTL after last use and a session count.
// Every Interval (default 2h) a sweep also drops those of issues done or cancelled
// for Grace (default 24h), the upstream GC defaults.
type SessionsConfig struct {
	TTL      string `json:"ttl"`
	Max      int    `json:"max"`
	Interval string `json:"interval,omitempty"`
	Grace    string `json:"grace,omitempty"`
}

// RelayConfig enables an embedded relay: Multica API, inference, MCP (ADR 0014, 0012, 0023).
// URL is what agents receive; the controller must be an approved peer answering it.
type RelayConfig struct {
	Listen string `json:"listen"`
	URL    string `json:"url"`
}

// validateCompaction prevents a configured cap from being silently ignored without
// managed inference, which supplies the selected model's context metadata.
func (c *OpenCodeConfig) validateCompaction() error {
	if c.CompactionMaxTokens < 0 {
		return fmt.Errorf("compaction_max_tokens must be nonnegative")
	}
	if c.CompactionMaxTokens > 0 && c.InferenceFile == "" {
		return fmt.Errorf("compaction_max_tokens requires inference_file")
	}
	return nil
}
