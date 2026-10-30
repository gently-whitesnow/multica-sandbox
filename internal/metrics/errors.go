package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
)

// ErrorType names why a request got no response, as the shared http.client contract does.
func ErrorType(ctx context.Context, err error) string {
	var dns *net.DNSError
	var record tls.RecordHeaderError
	var certificate *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var timeout interface{ Timeout() bool }
	var op *net.OpError
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return "request-canceled"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return "request-timeout"
	case errors.As(err, &dns):
		return "dns-resolve"
	case errors.As(err, &record) || errors.As(err, &certificate) || errors.As(err, &unknown):
		return "tls-handshake"
	case errors.As(err, &op):
		return "connection-failed"
	default:
		return "other"
	}
}
