package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

func validPanel(uuid string) panel.User {
	return panel.User{ID: 1, UUID: uuid, SpeedLimit: 2, DeviceLimit: 3, TCPLimit: 0}
}

func TestNormalizeUsers(t *testing.T) {
	users, stats, err := NormalizeUsers([]panel.User{
		validPanel("701534E1-8540-49F3-87AF-71680A1B97FF"),
		validPanel("701534E1-8540-49F3-87AF-71680A1B97FF"),    // duplicate uuid
		{ID: 1, UUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, // duplicate id
	}, UserOptions{AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].UUID != "701534e1-8540-49f3-87af-71680a1b97ff" {
		t.Fatalf("users = %+v", users)
	}
	if stats.Accepted != 1 || stats.Duplicates < 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestNormalizeProcessUUIDCollision(t *testing.T) {
	users, stats, err := NormalizeUsers([]panel.User{
		{ID: 1, UUID: "00000000-0000-0001-0000-000000000001", SpeedLimit: 0},
		{ID: 2, UUID: "00000000-0000-0002-0000-000000000001", SpeedLimit: 0},
	}, UserOptions{AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || stats.Duplicates != 1 {
		t.Fatalf("users=%+v stats=%+v", users, stats)
	}
}

func TestNormalizeEmptyAndMassRemove(t *testing.T) {
	prev := make([]xray.User, 0, 10)
	for i := int64(1); i <= 10; i++ {
		prev = append(prev, xray.User{ID: i, UUID: "00000000-0000-0000-0000-000000000000"})
	}
	if _, _, err := NormalizeUsers(nil, UserOptions{AllowEmpty: false}, prev); err == nil {
		t.Fatal("expected empty-list rejection")
	}
	if users, stats, err := NormalizeUsers(nil, UserOptions{AllowEmpty: true}, prev); err != nil || len(users) != 0 || stats.MassRemove {
		t.Fatalf("empty users=%v stats=%+v err=%v", users, stats, err)
	}
	if _, _, err := NormalizeUsers([]panel.User{validPanel("701534e1-8540-49f3-87af-71680a1b97ff")},
		UserOptions{AllowEmpty: true, MassRemoveGuard: "reject"}, prev); err == nil || !strings.Contains(err.Error(), "mass removal") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromPanelValidation(t *testing.T) {
	base := panel.Node{NodeType: "vless", Config: panel.Config{Port: 443, StreamType: "tcp", TLSType: "none"}}
	if _, err := FromPanel(base); err != nil {
		t.Fatal(err)
	}
	xtls := base
	xtls.Config.TLSType = "xtls"
	if _, err := FromPanel(xtls); err == nil || !strings.Contains(err.Error(), "xtls") {
		t.Fatalf("err = %v", err)
	}
	wsReality := base
	wsReality.Config.StreamType = "ws"
	wsReality.Config.TLSType = "reality"
	wsReality.Config.PrivateKey = "key"
	wsReality.Config.ServerNames = []string{"example.com"}
	wsReality.Config.Dest = "example.com:443"
	if _, err := (Spec{Config: wsReality.Config, NodeType: "vless"}).Xray(LocalRuntime{Listen: "127.0.0.1"}); err == nil {
		t.Fatal("expected ws+reality rejection")
	}
	visionNoTLS := base
	visionNoTLS.Config.Flow = "xtls-rprx-vision"
	if _, err := (Spec{Config: visionNoTLS.Config, NodeType: "vless"}).Xray(LocalRuntime{Listen: "127.0.0.1"}); err == nil {
		t.Fatal("expected vision without TLS to be rejected by xray validation")
	}
}

func TestInvalidThreshold(t *testing.T) {
	if _, _, err := NormalizeUsers([]panel.User{{ID: 1, UUID: "bad"}}, UserOptions{AllowEmpty: true}, nil); err == nil {
		t.Fatal("expected invalid-ratio rejection")
	}
}

func TestSpecExtendedFields(t *testing.T) {
	s := Spec{
		NodeType: "vless",
		Config: panel.Config{
			Port: 443, StreamType: "xhttp", TLSType: "none", Path: "/old",
			Decryption: "none",
			XHTTP: &panel.XHTTPConfig{Host: "cdn.example.com", Path: "/new", Mode: "packet-up",
				Headers: map[string]string{"X-Test": "1"}},
		},
	}
	xc, err := s.Xray(LocalRuntime{Listen: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if xc.XHTTP == nil || xc.XHTTP.Path != "/new" || xc.XHTTP.Mode != "packet-up" || xc.Decryption != "none" {
		t.Fatalf("xhttp = %+v", xc.XHTTP)
	}
}

func TestSpecPassesLocalCertificates(t *testing.T) {
	// Regression: multi-certificate configs (node.tls.certificates) must
	// reach the Xray config; dropping them silently disables SNI selection.
	certA, keyA := testCertPair(t, "a.test")
	certB, keyB := testCertPair(t, "b.test")
	s := Spec{
		NodeType: "vless",
		Config:   panel.Config{Port: 443, StreamType: "tcp", TLSType: "tls"},
	}
	xc, err := s.Xray(LocalRuntime{Listen: "127.0.0.1", Certificates: []xray.TLSCertificate{
		{CertPEM: certA, KeyPEM: keyA},
		{CertPEM: certB, KeyPEM: keyB},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(xc.Certificates) != 2 {
		t.Fatalf("certificates = %d, want 2", len(xc.Certificates))
	}
	if xc.Certificates[0].CertPEM != certA || xc.Certificates[1].CertPEM != certB {
		t.Fatal("certificate order not preserved")
	}
}

func testCertPair(t *testing.T, dnsName string) (certPEM, keyPEM string) {
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
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}
