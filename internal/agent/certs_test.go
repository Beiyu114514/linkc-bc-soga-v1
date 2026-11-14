package agent_test

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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
)

// writeCertFiles writes a self-signed certificate for dnsName into dir and
// returns the file paths plus the SHA-256 pin of the DER encoding.
func writeCertFiles(t *testing.T, dir, dnsName string) (certFile, keyFile, pin string) {
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
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return certFile, keyFile, hex.EncodeToString(sum[:])
}

func tlsNodeClient(t *testing.T, ctx context.Context, port int, serverName, pin string) *http.Client {
	t.Helper()
	socksPort := freePort(t)
	inst, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
		TLS: true, ServerName: serverName, PinnedPeerCertSha256: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	hc := socksHTTPClient(t, socksPort)
	// Every request must open a fresh tunnel: a pooled keep-alive connection
	// would survive the certificate rotation and hide the old certificate.
	hc.Transport.(*http.Transport).DisableKeepAlives = true
	return hc
}

// TestReloadCertsRotatesServingCertificate is the SIGHUP E2E: the certificate
// files are replaced on disk and ReloadCerts must hot-swap the inbound so the
// new certificate is served without a restart (M4 leftover).
func TestReloadCertsRotatesServingCertificate(t *testing.T) {
	ctx := context.Background()
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	certDir := t.TempDir()
	certFile, keyFile, pinA := writeCertFiles(t, certDir, "a.test")

	// The panel serves a TLS node; the certificate comes from local files.
	nc := p.Snapshot().Config
	nc.TLSType = "tls"
	p.SetNodeConfig(nc)

	cfg := testConfig(t, srv.URL, t.TempDir())
	cfg.Node.TLS.Certificates = []config.CertPair{{CertFile: certFile, KeyFile: keyFile}}
	a, stop, _ := startAgent(t, cfg)
	defer stop()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "cert-a")
	}))
	defer target.Close()

	hcA := tlsNodeClient(t, ctx, nodePort, "a.test", pinA)
	resp, err := hcA.Get(target.URL)
	if err != nil {
		t.Fatalf("initial TLS request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "cert-a" {
		t.Fatalf("body = %q", body)
	}

	// Replace the certificate on disk (same paths) and reload.
	_, _, pinB := writeCertFiles(t, certDir, "b.test")
	if err := a.ReloadCerts(); err != nil {
		t.Fatalf("ReloadCerts: %v", err)
	}
	if s := a.Snapshot(); !s.Ready || s.LastError != "" {
		t.Fatalf("agent unhealthy after reload: ready=%v err=%q", s.Ready, s.LastError)
	}

	// The new certificate must be served now.
	hcB := tlsNodeClient(t, ctx, nodePort, "b.test", pinB)
	resp, err = hcB.Get(target.URL)
	if err != nil {
		t.Fatalf("request after rotation: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "cert-a" {
		t.Fatalf("body = %q", body)
	}
	// The old certificate must be gone.
	if resp, err := hcA.Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("old certificate still served after rotation")
	}

	// A no-op reload (files unchanged) must be a cheap no-op.
	if err := a.ReloadCerts(); err != nil {
		t.Fatalf("no-op ReloadCerts: %v", err)
	}
	resp, err = hcB.Get(target.URL)
	if err != nil {
		t.Fatalf("request after no-op reload: %v", err)
	}
	resp.Body.Close()
}

// TestReloadCertsMissingFileKeepsOld verifies that a broken certificate file
// does not take the node down: the previous inbound keeps serving.
func TestReloadCertsMissingFileKeepsOld(t *testing.T) {
	ctx := context.Background()
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	certDir := t.TempDir()
	certFile, keyFile, pin := writeCertFiles(t, certDir, "a.test")
	nc := p.Snapshot().Config
	nc.TLSType = "tls"
	p.SetNodeConfig(nc)

	cfg := testConfig(t, srv.URL, t.TempDir())
	cfg.Node.TLS.Certificates = []config.CertPair{{CertFile: certFile, KeyFile: keyFile}}
	a, stop, _ := startAgent(t, cfg)
	defer stop()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "cert-a")
	}))
	defer target.Close()
	hc := tlsNodeClient(t, ctx, nodePort, "a.test", pin)
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("initial TLS request: %v", err)
	}
	resp.Body.Close()

	// Break the key file; the reload must fail and keep the old inbound.
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadCerts(); err == nil {
		t.Fatal("expected ReloadCerts to fail on a broken key file")
	}
	resp, err = hc.Get(target.URL)
	if err != nil {
		t.Fatalf("old inbound must keep serving: %v", err)
	}
	resp.Body.Close()
	if s := a.Snapshot(); !s.Ready {
		t.Fatal("agent must stay ready after a failed reload")
	}
}
