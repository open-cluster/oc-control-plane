package e2e

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTheTerminatorIsAcceptedOnlyByAClientPinnedToItsKey(t *testing.T) {
	t.Parallel()

	upstream := startEchoUpstream(t)
	terminator, err := StartTLSTerminator("localhost", upstream)
	if err != nil {
		t.Fatalf("starting the terminator: %v", err)
	}
	t.Cleanup(func() { _ = terminator.Close() })

	if terminator.SPKIPin == "" {
		t.Fatal("the terminator reported no pin; every later test would trust anything")
	}

	t.Run("the advertised pin is accepted", func(t *testing.T) {
		if err := dialPinned(terminator.Address, terminator.SPKIPin); err != nil {
			t.Errorf("a client pinned to the advertised key was refused: %v", err)
		}
	})

	t.Run("any other pin is refused", func(t *testing.T) {
		other := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))
		if err := dialPinned(terminator.Address, other); err == nil {
			t.Error("a client pinned to a different key was accepted; the pin is decorative")
		}
	})
}

func dialPinned(address, pin string) error {
	expected, err := base64.StdEncoding.DecodeString(pin)
	if err != nil {
		return err
	}

	connection, err := tls.Dial("tcp", address, &tls.Config{
		ServerName:         "localhost",
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			leaf, parseErr := x509.ParseCertificate(rawCerts[0])
			if parseErr != nil {
				return parseErr
			}
			presented := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if string(presented[:]) != string(expected) {
				return errors.New("server public key matches no pinned key")
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	return connection.Close()
}

func startEchoUpstream(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	return listener.Addr().String()
}
