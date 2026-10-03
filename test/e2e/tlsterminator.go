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
	"strings"
	"time"

	"golang.org/x/net/http2"
)

type TLSTerminator struct {
	SPKIPin string
	Address string

	server   *http.Server
	listener net.Listener
	acks     *acknowledgementProbe
}

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
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		return nil, fmt.Errorf("listening: %w", err)
	}

	acks := &acknowledgementProbe{}
	terminator := &TLSTerminator{
		SPKIPin:  pin,
		Address:  listener.Addr().String(),
		listener: listener,
		acks:     acks,
		server:   &http.Server{Handler: h2cReverseProxy(target, acks)},
	}
	go func() { _ = terminator.server.Serve(listener) }()
	return terminator, nil
}

func (t *TLSTerminator) Close() error {
	return t.server.Close()
}

func h2cReverseProxy(target *url.URL, acknowledgements *acknowledgementProbe) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") {
			response.Body = &acknowledgementBody{upstream: response.Body, probe: acknowledgements}
		}
		return nil
	}
	proxy.FlushInterval = -1
	return proxy
}

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
