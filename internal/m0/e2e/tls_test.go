package e2e

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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
)

func makeCert(t *testing.T, dnsName string) (certFile, keyFile, sha256Hex string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
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
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return certFile, keyFile, hex.EncodeToString(sum[:])
}

func TestVLESSTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	certFile, keyFile, pin := makeCert(t, "localhost")
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	nodePort := freePort(t)
	e, err := engine.Start(ctx, engine.NodeConfig{
		Listen: "0.0.0.0", Port: nodePort, StreamType: "tcp",
		TLSType: "tls", CertPEM: string(certPEM), KeyPEM: string(keyPEM),
		AllowPrivate: true,
	}, []engine.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatalf("start tls node: %v", err)
	}
	defer e.Close()
	time.Sleep(100 * time.Millisecond)

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: nodePort, UUID: testUUID,
		TLS: true, ServerName: "localhost", PinnedPeerCertSha256: pin,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()
	time.Sleep(100 * time.Millisecond)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls-ok")
	}))
	defer target.Close()
	resp, err := socksHTTPClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatalf("http over vless+tls: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tls-ok" {
		t.Fatalf("body = %q", body)
	}
	_ = fmt.Sprintf("%v", pin)
}
