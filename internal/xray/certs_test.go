package xray_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// makeSNICert creates a self-signed certificate for one DNS name and returns
// the PEM pair plus the SHA-256 pin of the DER encoding.
func makeSNICert(t *testing.T, dnsName string) (certPEM, keyPEM, pin string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		hex.EncodeToString(sum[:])
}

// TestStartMultiCertificateSNI serves two certificates on one inbound and
// verifies Xray selects the right one per SNI (M4 leftover: multi-cert/SNI
// real scenario).
func TestStartMultiCertificateSNI(t *testing.T) {
	certA, keyA, pinA := makeSNICert(t, "a.test")
	certB, keyB, pinB := makeSNICert(t, "b.test")
	if pinA == pinB {
		t.Fatal("test certificates must differ")
	}
	ctx := context.Background()
	port := freePort(t)
	node := baseNode(port)
	node.TLSType = "tls"
	node.Certificates = []xray.TLSCertificate{
		{CertPEM: certA, KeyPEM: keyA},
		{CertPEM: certB, KeyPEM: keyB},
	}
	e, err := xray.Start(ctx, node, []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "sni-ok")
	}))
	defer target.Close()

	for _, tc := range []struct {
		name       string
		serverName string
		pin        string
	}{
		{name: "first-cert", serverName: "a.test", pin: pinA},
		{name: "second-cert", serverName: "b.test", pin: pinB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socksPort := freePort(t)
			client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
				SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
				TLS: true, ServerName: tc.serverName, PinnedPeerCertSha256: tc.pin,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			resp, err := socksClient(t, socksPort).Get(target.URL)
			if err != nil {
				t.Fatalf("SNI %s: %v", tc.serverName, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "sni-ok" {
				t.Fatalf("body = %q", body)
			}
		})
	}

	// A pin for the wrong certificate must fail: proves the server really
	// selects per SNI instead of always sending the first certificate.
	socksPort := freePort(t)
	client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
		TLS: true, ServerName: "b.test", PinnedPeerCertSha256: pinA,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if resp, err := socksClient(t, socksPort).Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("wrong certificate pin must fail the handshake")
	}
}
