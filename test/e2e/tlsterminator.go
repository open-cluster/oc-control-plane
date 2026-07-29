package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"golang.org/x/net/http2"
)

// TLSTerminator fronts a plaintext HTTP/2 control-plane endpoint with TLS, which is how the
// Relay reaches one in production: the Relay pins the edge's public key and refuses anything
// else, while the control plane behind it speaks h2c. Connecting the two directly is not
// possible and not desirable — it would remove the layer this exercise exists to test.
type TLSTerminator struct {
	// SPKIPin is the Relay's trust anchor for this endpoint: standard base64 of the
	// SHA-256 over the certificate's SubjectPublicKeyInfo.
	SPKIPin string
	// Address is the host:port the Relay dials.
	Address string

	server   *http.Server
	listener net.Listener
}

// StartTLSTerminator listens on an ephemeral port and forwards to upstream over h2c, using a
// freshly generated certificate for serverName. The certificate is generated per run rather
// than fixed, so a test can never pass by trusting a key some earlier run left behind.
func StartTLSTerminator(serverName, upstream string) (*TLSTerminator, error) {
	certificate, pin, err := selfSignedCertificate(serverName)
	if err != nil {
		return nil, err
	}

	target, err := url.Parse("http://" + upstream)
	if err != nil {
		return nil, fmt.Errorf("parsing upstream %q: %w", upstream, err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		// gRPC requires HTTP/2, and negotiating it over ALPN is how a real edge does it.
		NextProtos: []string{"h2"},
	})
	if err != nil {
		return nil, fmt.Errorf("listening: %w", err)
	}

	terminator := &TLSTerminator{
		SPKIPin:  pin,
		Address:  listener.Addr().String(),
		listener: listener,
		server:   &http.Server{Handler: h2cReverseProxy(target)},
	}
	go func() { _ = terminator.server.Serve(listener) }()
	return terminator, nil
}

// Close stops accepting and releases the port.
func (t *TLSTerminator) Close() error {
	return t.server.Close()
}

// h2cReverseProxy forwards to an upstream that speaks HTTP/2 without TLS. gRPC needs HTTP/2
// end to end — downgrading to HTTP/1.1 across the proxy would break streaming, which is
// most of the protocol.
func h2cReverseProxy(target *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
	}
	proxy.FlushInterval = -1 // Stream responses through rather than buffering them.
	return proxy
}

// selfSignedCertificate returns a certificate for serverName and the SPKI pin a Relay must
// be configured with to accept it. The pin is computed the way the Relay computes it, from
// the certificate's SubjectPublicKeyInfo, so the two agree by construction rather than by a
// value copied between them.
func selfSignedCertificate(serverName string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generating key: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: serverName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{serverName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("creating certificate: %w", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parsing certificate: %w", err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed},
		base64.StdEncoding.EncodeToString(digest[:]), nil
}
