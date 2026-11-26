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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
	xproxy "golang.org/x/net/proxy"
)

const testUUID = "701534e1-8540-49f3-87af-71680a1b97ff"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func testCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return string(cert), string(keyPEM)
}

func baseNode(port int) xray.NodeConfig {
	return xray.NodeConfig{Listen: "127.0.0.1", Port: port, StreamType: "tcp", TLSType: "none", AllowPrivate: true}
}

func TestBuildConfigMatrix(t *testing.T) {
	cert, key := testCert(t)
	node := baseNode(443)
	if _, err := xray.BuildConfig(node, nil, "vless-1"); err != nil {
		t.Fatal(err)
	}
	wsTLS := node
	wsTLS.StreamType = "ws"
	wsTLS.TLSType = "tls"
	wsTLS.Path = "/ws"
	wsTLS.CertPEM, wsTLS.KeyPEM = cert, key
	raw, err := xray.BuildConfig(wsTLS, nil, "vless-1")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	in := cfg["inbounds"].([]any)[0].(map[string]any)
	stream := in["streamSettings"].(map[string]any)
	if stream["network"] != "ws" || stream["security"] != "tls" {
		t.Fatalf("stream = %+v", stream)
	}
	reality := node
	reality.TLSType = "reality"
	reality.PrivateKey = "private"
	reality.ServerNames = []string{"example.com"}
	reality.Dest = "example.com:443"
	// An empty short_id is the documented way to disable shortIds; Xray
	// accepts it (it hex-decodes to an all-zero id).
	reality.ShortIDs = []string{""}
	if _, err := xray.BuildConfig(reality, nil, "vless-1"); err != nil {
		t.Fatal(err)
	}

	bad := []xray.NodeConfig{
		func() xray.NodeConfig { n := node; n.StreamType = "h2"; return n }(),
		func() xray.NodeConfig {
			n := node
			n.StreamType = "ws"
			n.TLSType = "reality"
			n.PrivateKey = "k"
			n.ServerNames = []string{"a"}
			n.Dest = "a:443"
			return n
		}(),
		func() xray.NodeConfig { n := node; n.Flow = "xtls-rprx-vision"; return n }(),
		func() xray.NodeConfig { n := node; n.TLSType = "tls"; return n }(),
	}
	for i, n := range bad {
		if _, err := xray.BuildConfig(n, nil, "vless-1"); err == nil {
			t.Fatalf("case %d expected error", i)
		}
	}
}

func socksClient(t *testing.T, socksPort int) *http.Client {
	t.Helper()
	d, err := xproxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		t.Fatal("not context dialer")
	}
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: cd.DialContext}}
}

func TestStartTrafficAndHotUsers(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	e, err := xray.Start(ctx, baseNode(port), []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	socksPort := freePort(t)
	client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "production-xray-ok")
	}))
	defer target.Close()

	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "production-xray-ok" {
		t.Fatalf("body = %q", body)
	}
	tr := e.ReadTraffic()
	if tr[1].Up == 0 && tr[1].Down == 0 {
		t.Fatalf("no traffic: %+v", tr)
	}

	if err := e.ApplyUsers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := socksClient(t, socksPort).Get(target.URL); err == nil {
		t.Fatal("removed user should not connect")
	}
	if err := e.ApplyUsers(ctx, []xray.User{{ID: 1, UUID: testUUID}}); err != nil {
		t.Fatal(err)
	}
	resp, err = socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestBuildConfigRedactionReality(t *testing.T) {
	// Ensure the security/transport mapping is emitted, not just accepted.
	n := baseNode(443)
	n.TLSType = "reality"
	n.PrivateKey = strings.Repeat("a", 43)
	n.ServerNames = []string{"www.example.com"}
	n.Dest = "www.example.com:443"
	n.ShortIDs = []string{"abcd"}
	raw, err := xray.BuildConfig(n, nil, "vless-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"security":"reality"`) || !strings.Contains(raw, `"privateKey"`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestStartWSTLS(t *testing.T) {
	cert, key := testCert(t)
	block, _ := pem.Decode([]byte(cert))
	if block == nil {
		t.Fatal("bad cert")
	}
	sum := sha256.Sum256(block.Bytes)
	pin := hex.EncodeToString(sum[:])
	port := freePort(t)
	ctx := context.Background()
	node := baseNode(port)
	node.StreamType = "ws"
	node.TLSType = "tls"
	node.Path = "/ws"
	node.CertPEM, node.KeyPEM = cert, key
	e, err := xray.Start(ctx, node, []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	socksPort := freePort(t)
	client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
		Network: "ws", Path: "/ws", TLS: true, ServerName: "localhost", PinnedPeerCertSha256: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ws-tls-ok")
	}))
	defer target.Close()
	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ws-tls-ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestBuildConfigExtendedTransport(t *testing.T) {
	cert, key := testCert(t)
	ech := "ACD2AjC5yuBQ3430geNDLdIRndloCry/DBb9ZeYp0TmeowBB/g0APQAAIAAgc4c5FLxJySxZAPRHs8CDCbuz1h76I9pLhctC/kgScAAEAAEAAQAOcHVibGljLmV4YW1wbGUAAA=="
	n := baseNode(443)
	n.StreamType = "xhttp"
	n.TLSType = "tls"
	n.CertPEM, n.KeyPEM = cert, key
	n.ECHServerKeys = ech
	n.Decryption = "none"
	n.XHTTP = &xray.XHTTPConfig{Host: "cdn.example.com", Path: "/x", Mode: "packet-up",
		Headers: map[string]string{"X-Test": "1"}}
	raw, err := xray.BuildConfig(n, nil, "vless-1")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(raw), &cfg)
	in := cfg["inbounds"].([]any)[0].(map[string]any)
	stream := in["streamSettings"].(map[string]any)
	xh := stream["xhttpSettings"].(map[string]any)
	if xh["mode"] != "packet-up" || xh["host"] != "cdn.example.com" || xh["path"] != "/x" {
		t.Fatalf("xhttp = %+v", xh)
	}
	tlsSettings := stream["tlsSettings"].(map[string]any)
	if tlsSettings["echServerKeys"] != ech {
		t.Fatalf("ech = %v", tlsSettings["echServerKeys"])
	}

	g := baseNode(443)
	g.StreamType = "grpc"
	g.TLSType = "none"
	g.ServiceName = "svc"
	g.GRPC = &xray.GRPCConfig{Authority: "a.example.com", MultiMode: true, IdleTimeout: 30,
		HealthCheckTimeout: 10, PermitWithoutStream: true, InitialWindowsSize: 65535, UserAgent: "ua"}
	raw, err = xray.BuildConfig(g, nil, "vless-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	stream = cfg["inbounds"].([]any)[0].(map[string]any)["streamSettings"].(map[string]any)
	gs := stream["grpcSettings"].(map[string]any)
	if gs["authority"] != "a.example.com" || gs["multiMode"] != true || gs["serviceName"] != "svc" {
		t.Fatalf("grpc = %+v", gs)
	}

	bad := n
	bad.TLSType = "reality"
	bad.ECHServerKeys = ech
	bad.PrivateKey = "x"
	bad.ServerNames = []string{"a"}
	bad.Dest = "a:443"
	if _, err := xray.BuildConfig(bad, nil, "vless-1"); err == nil {
		t.Fatal("expected ECH+REALITY rejection")
	}
}

func certPin(t *testing.T, cert string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(cert))
	if block == nil {
		t.Fatal("bad cert")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func TestStartXHTTPAndGRPC(t *testing.T) {
	cert, key := testCert(t)
	pin := certPin(t, cert)
	cases := []struct {
		name    string
		network string
		path    string
		service string
	}{
		{name: "xhttp", network: "xhttp", path: "/x"},
		{name: "grpc", network: "grpc", service: "svc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			node := baseNode(port)
			node.StreamType = tc.network
			node.TLSType = "tls"
			node.CertPEM, node.KeyPEM = cert, key
			node.Path, node.ServiceName = tc.path, tc.service
			ctx := context.Background()
			e, err := xray.Start(ctx, node, []xray.User{{ID: 1, UUID: testUUID}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			socksPort := freePort(t)
			c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
				SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
				Network: tc.network, Path: tc.path, ServiceName: tc.service,
				TLS: true, ServerName: "localhost", PinnedPeerCertSha256: pin,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.name+"-ok")
			}))
			defer target.Close()
			resp, err := socksClient(t, socksPort).Get(target.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != tc.name+"-ok" {
				t.Fatalf("body = %q", body)
			}
		})
	}
}

func TestStartUserSpeedLimit(t *testing.T) {
	port := freePort(t)
	ctx := context.Background()
	e, err := xray.Start(ctx, baseNode(port), []xray.User{{ID: 1, UUID: testUUID, SpeedLimit: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	socksPort := freePort(t)
	c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := make([]byte, 512*1024)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer target.Close()
	start := time.Now()
	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if len(body) != len(payload) || elapsed < 400*time.Millisecond {
		t.Fatalf("len=%d elapsed=%v with 4 Mbps limit", len(body), elapsed)
	}
}

func TestBuildConfigMultipleCertificates(t *testing.T) {
	cert1, key1 := testCert(t)
	cert2, key2 := testCert(t)
	n := baseNode(443)
	n.TLSType = "tls"
	n.Certificates = []xray.TLSCertificate{{CertPEM: cert1, KeyPEM: key1}, {CertPEM: cert2, KeyPEM: key2}}
	raw, err := xray.BuildConfig(n, nil, "vless-1")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal([]byte(raw), &cfg)
	in := cfg["inbounds"].([]any)[0].(map[string]any)
	tls := in["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
	certs := tls["certificates"].([]any)
	if len(certs) != 2 {
		t.Fatalf("certificates = %d", len(certs))
	}
}

func TestApplyUsersConcurrentWithSwapInbound(t *testing.T) {
	ctx := context.Background()
	e, err := xray.Start(ctx, baseNode(freePort(t)), []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 9)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			if err := e.ApplyUsers(ctx, []xray.User{{ID: 1, UUID: testUUID, SpeedLimit: float64(i + 1)}}); err != nil {
				errs <- fmt.Errorf("ApplyUsers(%d): %w", i, err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			if _, err := e.SwapInbound(ctx, baseNode(freePort(t)), []xray.User{{ID: 1, UUID: testUUID}}); err != nil {
				errs <- fmt.Errorf("SwapInbound(%d): %w", i, err)
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestDeviceLimitZeroToPositiveNoUnderflow(t *testing.T) {
	port := freePort(t)
	ctx := context.Background()
	e, err := xray.Start(ctx, baseNode(port), []xray.User{{ID: 1, UUID: testUUID, DeviceLimit: 0}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	socksPort := freePort(t)
	c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	done := make(chan error, 1)
	go func() {
		resp, err := socksClient(t, socksPort).Get(target.URL)
		if err != nil {
			done <- err
			return
		}
		resp.Body.Close()
		done <- nil
	}()
	<-entered

	// Raise device limit from 0 -> 1 while the first connection is active.
	if err := e.ApplyUsers(ctx, []xray.User{{ID: 1, UUID: testUUID, DeviceLimit: 1}}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Ensure device counters never underflow back above 0 after close.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Gate().Metrics().Devices == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.Gate().Metrics().Devices; got != 0 {
		t.Fatalf("active devices after close = %d, want 0", got)
	}
}

func TestReadTrafficDrainsRemovedUserBytes(t *testing.T) {
	port := freePort(t)
	ctx := context.Background()
	e, err := xray.Start(ctx, baseNode(port), []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	socksPort := freePort(t)
	c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer target.Close()
	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Remove user 1 BEFORE ReadTraffic is called.
	if err := e.ApplyUsers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	stats := e.ReadTraffic()
	if stats[1].Up+stats[1].Down == 0 {
		t.Fatalf("expected non-zero drained traffic for removed user 1, got %+v", stats)
	}
}

func TestFreedomBlocksPrivateTargetWhenDisabled(t *testing.T) {
	port := freePort(t)
	n := baseNode(port)
	n.AllowPrivate = false
	ctx := context.Background()
	e, err := xray.Start(ctx, n, []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	socksPort := freePort(t)
	c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should-not-reach")
	}))
	defer target.Close()
	if resp, err := socksClient(t, socksPort).Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("expected loopback target to be blocked when AllowPrivate=false")
	}
}

func TestReadTrafficKeepsRetiredUserWithActiveUDPAndOffMode(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mode   string
		target xnet.Destination
	}{
		{
			name:   "udp_enforce",
			mode:   "enforce",
			target: xnet.UDPDestination(xnet.ParseAddress("8.8.8.8"), 53),
		},
		{
			name:   "tcp_off",
			mode:   "off",
			target: xnet.TCPDestination(xnet.ParseAddress("1.1.1.1"), 443),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := baseNode(freePort(t))
			n.LimitsMode = tc.mode
			e, err := xray.Start(ctx, n, []xray.User{{ID: 1, UUID: testUUID}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()

			connCtx := session.ContextWithInbound(ctx, &session.Inbound{
				User:   &protocol.MemoryUser{Email: "u1"},
				Source: xnet.TCPDestination(xnet.ParseAddress("1.2.3.4"), 12345),
			})
			connCtx = session.ContextWithOutbounds(connCtx, []*session.Outbound{{Target: tc.target}})
			link := &transport.Link{}
			if !e.Gate().Before(connCtx, link) {
				t.Fatal("Gate.Before rejected active connection")
			}

			// Retire user 1 while session is still active.
			if err := e.ApplyUsers(ctx, nil); err != nil {
				t.Fatal(err)
			}
			_ = e.ReadTraffic()
			if got := e.Gate().ActiveConns(1); got != 1 {
				t.Fatalf("ActiveConns = %d, want 1", got)
			}

			// Simulate traffic arriving on the still-open session after a zero-delta tick.
			e.Gate().After(connCtx, link)
			if got := e.Gate().ActiveConns(1); got != 0 {
				t.Fatalf("ActiveConns after close = %d, want 0", got)
			}
			_ = e.ReadTraffic()
		})
	}
}
