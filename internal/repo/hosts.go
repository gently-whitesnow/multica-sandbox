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
	"slices"
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
// DefaultEmail. API, with a password file, is the forge API origin (for example
// https://api.github.com) that gh reaches through the forge relay.
type Host struct {
	WorkspaceID  string `json:"workspace_id"`
	Host         string `json:"host"`
	Upstream     string `json:"upstream,omitempty"`
	Username     string `json:"username,omitempty"`
	PasswordFile string `json:"password_file,omitempty"`
	CommitName   string `json:"commit_name,omitempty"`
	CommitEmail  string `json:"commit_email,omitempty"`
	API          string `json:"api,omitempty"`
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
	api                *url.URL
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
		upstreamOrigin, valid := httpOrigin(upstream, c.AllowHTTP)
		var api *url.URL
		if host.API != "" {
			// gh reaches the API by the forge's own name, so the host cannot carry a port.
			var ok bool
			api, ok = httpOrigin(host.API, c.AllowHTTP)
			valid = valid && ok && host.PasswordFile != "" && !strings.Contains(host.Host, ":")
		}
		if !uuid.MatchString(host.WorkspaceID) || !hostName.MatchString(host.Host) || !valid || h.bindings[key].origin != nil ||
			(host.PasswordFile != "" && (!filepath.IsAbs(host.PasswordFile) || host.Username == "" || strings.ContainsAny(host.Username, ":\r\n\x00"))) ||
			(host.PasswordFile == "" && host.Username != "") || (host.CommitName == "") != (host.CommitEmail == "") ||
			!plain(host.CommitName) || !plain(host.CommitEmail) || strings.ContainsAny(host.CommitEmail, " <>") {
			return nil, fmt.Errorf("Git host %.64q: unique workspace and host, HTTPS upstream and API origins, a username with an absolute password file and a plain commit name with email required", host.Host)
		}
		h.bindings[key] = binding{origin: upstreamOrigin, username: host.Username, password: host.PasswordFile, name: host.CommitName, email: host.CommitEmail, api: api}
	}
	return h, nil
}

func httpOrigin(raw string, allowHTTP bool) (*url.URL, bool) {
	u, err := url.Parse(raw)
	valid := err == nil && (u.Scheme == "https" || (allowHTTP && u.Scheme == "http")) && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
	if !valid {
		return nil, false
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, true
}

// APIName is the name gh calls for a forge host: api.github.com for github.com, the
// host itself (Enterprise Server, /api/v3) otherwise.
func APIName(host string) string {
	if host == "github.com" {
		return "api.github.com"
	}
	return host
}

// APINames lists the forge API names of every binding with an API, sorted.
func (h *Hosts) APINames() []string {
	names := []string{}
	for key, b := range h.bindings {
		if b.api != nil {
			names = append(names, APIName(key[1]))
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// apiHosts lists the workspace's forge hosts with an API.
func (h *Hosts) apiHosts(workspace string) []string {
	hosts := []string{}
	for key, b := range h.bindings {
		if key[0] == workspace && b.api != nil {
			hosts = append(hosts, key[1])
		}
	}
	slices.Sort(hosts)
	return hosts
}

// api resolves the workspace's forge API by the name gh called, with a token header.
func (h *Hosts) api(workspace, name string) (*url.URL, string, bool) {
	for _, host := range h.apiHosts(workspace) {
		if APIName(host) == name {
			b := h.bindings[[2]string{workspace, host}]
			password, ok := secret(b.password)
			return b.api, "token " + password, ok
		}
	}
	return nil, "", false
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
	password, ok := secret(b.password)
	if !ok {
		return nil, "", false
	}
	return b.origin, "Basic " + basic(b.username, password), true
}

// secret rereads a password file, so external issuers can rotate it.
func secret(path string) (string, bool) {
	data, err := os.ReadFile(path)
	password := strings.TrimSpace(string(data))
	return password, err == nil && len(data) <= 4096 && password != "" && !strings.ContainsAny(password, "\r\n\x00")
}
