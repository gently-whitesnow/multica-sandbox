// Package repo serves upstream repository checkout inside attempts and mediates their
// Git smart HTTP (ADR 0015). Host credentials stay in controller memory and files.
package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config binds Git hosts to workspace-scoped credentials. Files are reread per request,
// so external issuers can rotate them.
type Config struct {
	Version   int    `json:"version"`
	AllowHTTP bool   `json:"allow_http,omitempty"`
	Hosts     []Host `json:"hosts"`
}

// Host is the claim URL host, e.g. github.com. Upstream defaults to https://<host>.
// PasswordFile, with Username, sends HTTP basic authentication; without it the host
// is fetched anonymously. CommitName and CommitEmail are the commit identity of its
// checkouts, for example a GitHub App bot; they default to the agent name and
// DefaultEmail.
type Host struct {
	WorkspaceID  string `json:"workspace_id"`
	Host         string `json:"host"`
	Upstream     string `json:"upstream,omitempty"`
	Username     string `json:"username,omitempty"`
	PasswordFile string `json:"password_file,omitempty"`
	CommitName   string `json:"commit_name,omitempty"`
	CommitEmail  string `json:"commit_email,omitempty"`
}

// DefaultEmail is the commit email of hosts without a configured identity.
const DefaultEmail = "agent@multica-sandbox.invalid"

var (
	uuid     = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
)

type binding struct {
	origin             *url.URL
	username, password string
	name, email        string
}

// Hosts resolves (workspace, host) to an upstream origin and authorization.
type Hosts struct{ bindings map[[2]string]binding }

func ReadConfig(path string) (*Hosts, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Git host configuration: %w", err)
	}
	defer f.Close()
	var c Config
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid Git host configuration")
	}
	return NewHosts(c)
}

func NewHosts(c Config) (*Hosts, error) {
	if c.Version != 1 || len(c.Hosts) == 0 || len(c.Hosts) > 256 {
		return nil, errors.New("Git host configuration version 1 with 1-256 hosts required")
	}
	h := &Hosts{bindings: map[[2]string]binding{}}
	for _, host := range c.Hosts {
		key := [2]string{host.WorkspaceID, host.Host}
		upstream := host.Upstream
		if upstream == "" {
			upstream = "https://" + host.Host
		}
		origin, err := url.Parse(upstream)
		valid := err == nil && (origin.Scheme == "https" || (c.AllowHTTP && origin.Scheme == "http")) && origin.Host != "" &&
			origin.User == nil && origin.RawQuery == "" && origin.Fragment == "" && (origin.Path == "" || origin.Path == "/")
		if !uuid.MatchString(host.WorkspaceID) || !hostName.MatchString(host.Host) || !valid || h.bindings[key] != (binding{}) ||
			(host.PasswordFile != "" && (!filepath.IsAbs(host.PasswordFile) || host.Username == "" || strings.ContainsAny(host.Username, ":\r\n\x00"))) ||
			(host.PasswordFile == "" && host.Username != "") || (host.CommitName == "") != (host.CommitEmail == "") ||
			!plain(host.CommitName) || !plain(host.CommitEmail) || strings.ContainsAny(host.CommitEmail, " <>") {
			return nil, fmt.Errorf("Git host %.64q: unique workspace and host, an HTTPS upstream origin, a username with an absolute password file and a plain commit name with email required", host.Host)
		}
		h.bindings[key] = binding{origin: &url.URL{Scheme: origin.Scheme, Host: origin.Host}, username: host.Username, password: host.PasswordFile, name: host.CommitName, email: host.CommitEmail}
	}
	return h, nil
}

// Has reports whether the workspace has a binding for host.
func (h *Hosts) Has(workspace, host string) bool {
	_, ok := h.bindings[[2]string{workspace, host}]
	return ok
}

// Identity is the commit identity for a claim repository URL of the workspace.
func (h *Hosts) Identity(workspace, repository, agent string) (string, string) {
	if host, _, ok := repoKey(repository); ok && h != nil {
		if b := h.bindings[[2]string{workspace, host}]; b.name != "" {
			return b.name, b.email
		}
	}
	name := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, agent)), " ")
	if name == "" || len(name) > 256 {
		name = "Multica agent"
	}
	return name, DefaultEmail
}

func plain(s string) bool {
	return len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '<' || r == '>' })
}

// resolve returns the upstream origin and the Authorization value, empty for anonymous hosts.
func (h *Hosts) resolve(workspace, host string) (*url.URL, string, bool) {
	b, ok := h.bindings[[2]string{workspace, host}]
	if !ok {
		return nil, "", false
	}
	if b.password == "" {
		return b.origin, "", true
	}
	data, err := os.ReadFile(b.password)
	password := strings.TrimSpace(string(data))
	if err != nil || len(data) > 4096 || password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return nil, "", false
	}
	return b.origin, "Basic " + basic(b.username, password), true
}
