package repo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const (
	// ForgePort is where attempts call forge APIs: the helper forwards loopback 443.
	ForgePort = "443"
	// CertDir holds the forge relay CA inside attempts (SSL_CERT_DIR).
	CertDir = "/workspace/certs"
	// CAPath is the projected CA certificate.
	CAPath = CertDir + "/multica-sandbox-forge.pem"
)

// Forge relays forge API calls of the unchanged gh CLI (ADR 0015). Attempts resolve
// the API names of bound hosts to loopback, the helper forwards them here, and Forge
// terminates TLS with leaf certificates from a per-process CA that attempts trust. It
// authorizes the attempt's Git relay credential, sent as the gh token, and attaches
// the workspace's host password, as native gh uses host credentials.
type Forge struct {
	relay  *Relay
	ca     *x509.Certificate
	key    *ecdsa.PrivateKey
	caPEM  []byte
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func NewForge(g *Relay) (*Forge, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "multica-sandbox forge relay"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign, PermittedDNSDomainsCritical: true, PermittedDNSDomains: g.hosts.APINames()}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	f := &Forge{relay: g, ca: ca, key: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), leaves: map[string]*tls.Certificate{}}
	g.ca = f.caPEM
	return f, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// TLSConfig serves only the configured API names; the CA's name constraints repeat it.
func (f *Forge) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := strings.ToLower(hello.ServerName)
		if !slices.Contains(f.relay.hosts.APINames(), name) {
			return nil, errors.New("unknown forge API name")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if leaf := f.leaves[name]; leaf != nil {
			return leaf, nil
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
			NotBefore: now.Add(-time.Hour), NotAfter: f.ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, f.ca, &key.PublicKey, f.key)
		if err != nil {
			return nil, err
		}
		leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
		f.leaves[name] = leaf
		return leaf, nil
	}}
}

// Authorize admits a granted attempt's gh call to its workspace's forge API by the
// TLS server name; Enterprise Server names serve only /api/.
func (f *Forge) Authorize(r *http.Request) (relay.Lease, bool) {
	if r.TLS == nil {
		return relay.Lease{}, false
	}
	name := strings.ToLower(r.TLS.ServerName)
	// gh sends "token <GH_TOKEN>"; the grant store reads bearer credentials.
	if opaque, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token "); ok {
		r.Header.Set("Authorization", "Bearer "+opaque)
	}
	lease, ok := f.relay.grants.Authorize(r)
	if !ok {
		return lease, false
	}
	workspace, granted := f.relay.workspace(lease.Attempt)
	origin, authorization, resolved := f.relay.hosts.api(workspace, name)
	if !granted || !resolved || strings.ToLower(r.Host) != name || (name != "api.github.com" && !strings.HasPrefix(r.URL.Path, "/api/")) {
		lease.Release()
		return relay.Lease{}, false
	}
	lease.Origin, lease.Path, lease.Authorization = origin, r.URL.Path, authorization
	return lease, true
}

// ForgePath admits API paths of bounded length.
func ForgePath(p string) bool { return strings.HasPrefix(p, "/") && len(p) <= 2048 }
