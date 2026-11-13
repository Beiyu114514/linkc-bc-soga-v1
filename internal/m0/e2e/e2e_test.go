package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/node"
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

func localIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Log("no non-loopback IPv4; falling back to 127.0.0.1 (OnlineMap will ignore it)")
	return "127.0.0.1"
}

func user(id int64) mockpanel.User {
	return mockpanel.User{
		ID: id, UUID: testUUID, SpeedLimit: 0, DeviceLimit: 0,
		TransferEnable: 1 << 40, Enable: true,
	}
}

func newPanel(t *testing.T, port int) (*mockpanel.Panel, *httptest.Server) {
	t.Helper()
	p := mockpanel.New(mockpanel.Config{
		APIKey:       "test-key",
		NodeID:       7,
		NodeType:     "vless",
		PullInterval: 60,
		PushInterval: 30,
		Port:         port,
		StreamType:   "tcp",
		TLSType:      "none",
		TrafficRate:  1,
	})
	p.SetUsers(user(1))
	srv := httptest.NewServer(p)
	return p, srv
}

func socksHTTPClient(t *testing.T, socksPort int) *http.Client {
	t.Helper()
	d, err := xproxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		t.Fatal("socks dialer is not ContextDialer")
	}
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: cd.DialContext,
		},
	}
}

func TestVLESSPlainTrafficOnlineStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host := localIPv4(t)
	nodePort := freePort(t)
	p, panelSrv := newPanel(t, nodePort)
	defer panelSrv.Close()

	n := node.New(node.Config{
		PanelURL: panelSrv.URL, APIKey: "test-key", NodeID: "7",
		NodeType: "vless", Listen: "0.0.0.0",
	})
	defer n.Close()
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("node sync: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello-through-vless")
	}))
	defer target.Close()

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: host, Port: nodePort, UUID: testUUID,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()
	time.Sleep(150 * time.Millisecond)

	hc := socksHTTPClient(t, socksPort)
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("http through vless: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello-through-vless" {
		t.Fatalf("body = %q", body)
	}

	// Traffic counters should have data. Poll briefly because the inbound link
	// counters update asynchronously with connection teardown.
	var deltas map[int64]engine.Traffic
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		deltas = n.Engine().ReadTraffic()
		if deltas[1].Up+deltas[1].Down > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if deltas[1].Up+deltas[1].Down == 0 {
		t.Fatalf("no traffic observed: %+v", deltas)
	}
	if err := n.PushTraffic(ctx, deltas); err != nil {
		t.Fatalf("push traffic: %v", err)
	}
	snap := p.Snapshot()
	if snap.Users[1].U+snap.Users[1].D == 0 {
		t.Fatalf("panel did not record traffic: %+v", snap.Users[1])
	}
	if len(snap.Online) != 1 || snap.Online[0].OnlineUser != 1 {
		t.Fatalf("panel online log = %+v", snap.Online)
	}

	// Online IP: hold a connection open through the node, then read OnlineMap.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open until the test closes it.
			go func() { io.Copy(io.Discard, c) }()
		}
	}()
	dialer, _ := xproxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xproxy.Direct)
	hold, err := dialer.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("hold dial: %v", err)
	}
	_, _ = hold.Write([]byte("x"))
	defer hold.Close()

	time.Sleep(300 * time.Millisecond)
	ips := n.Engine().OnlineIPs()
	if host == "127.0.0.1" {
		// Xray excludes loopback from OnlineMap by design; this environment
		// has no non-loopback IPv4, so only validate traffic/status here.
		t.Log("OnlineMap skipped: only loopback available")
	} else {
		if len(ips[1]) == 0 {
			t.Fatalf("OnlineMap empty for user: %+v", ips)
		}
		if !strings.Contains(strings.Join(ips[1], ","), host) {
			t.Fatalf("OnlineMap = %v, want source %s", ips[1], host)
		}
		if err := n.PushAliveIP(ctx, ips); err != nil {
			t.Fatalf("push alive: %v", err)
		}
		if got := len(p.Snapshot().Alive); got == 0 {
			t.Fatal("panel alive_ip empty")
		}
	}

	// Status uses load = CPU fraction; verify the site formula gives the
	// original percentage.
	st := node.Status{CPUPercent: 23.5, Uptime: 123, MemTotal: 100, MemUsed: 50}
	if err := n.PushStatus(ctx, st); err != nil {
		t.Fatalf("push status: %v", err)
	}
	info := p.Snapshot().InfoLogs
	if len(info) != 1 {
		t.Fatalf("info logs = %+v", info)
	}
	var frac float64
	if _, err := fmt.Sscanf(info[0].Load, "%f", &frac); err != nil {
		t.Fatalf("load %q: %v", info[0].Load, err)
	}
	if got := frac * 100; got != 23.5 {
		t.Fatalf("load display = %v%%, want 23.5%% (load=%q)", got, info[0].Load)
	}
	if info[0].Uptime != 123 {
		t.Fatalf("uptime = %v", info[0].Uptime)
	}
}

func TestUserHotRemoveAndAdd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host := localIPv4(t)
	nodePort := freePort(t)
	p, panelSrv := newPanel(t, nodePort)
	defer panelSrv.Close()

	n := node.New(node.Config{
		PanelURL: panelSrv.URL, APIKey: "test-key", NodeID: "7",
		NodeType: "vless", Listen: "0.0.0.0",
	})
	defer n.Close()
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: host, Port: nodePort, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	time.Sleep(100 * time.Millisecond)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	hc := socksHTTPClient(t, socksPort)
	if _, err := hc.Get(target.URL); err != nil {
		t.Fatalf("before remove: %v", err)
	}

	p.SetUsers() // empty list is legitimate
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("sync empty: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	hc2 := socksHTTPClient(t, socksPort)
	if _, err := hc2.Get(target.URL); err == nil {
		t.Fatal("expected authentication failure after user removal")
	}

	p.SetUsers(user(1))
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("sync add: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := socksHTTPClient(t, socksPort).Get(target.URL); err != nil {
		t.Fatalf("after re-add: %v", err)
	}
}

func TestNodeETagAndNodeTypeValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodePort := freePort(t)
	p, panelSrv := newPanel(t, nodePort)
	defer panelSrv.Close()

	n := node.New(node.Config{
		PanelURL: panelSrv.URL, APIKey: "test-key", NodeID: "7",
		NodeType: "vless", Listen: "127.0.0.1",
	})
	defer n.Close()
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("second sync (ETag): %v", err)
	}
	reqs := p.Snapshot().Requests
	if reqs["GET /api/v1/node"] < 2 || reqs["GET /api/v1/users"] < 2 {
		t.Fatalf("requests = %+v", reqs)
	}

	p.SetNodeConfig(mockpanel.Config{
		NodeID: 7, APIKey: "test-key", NodeType: "vmess", Port: nodePort,
		PullInterval: 60, PushInterval: 30, StreamType: "tcp", TLSType: "none",
	})
	if err := n.Sync(ctx); err == nil {
		t.Fatal("expected node_type mismatch")
	}
}

func TestInboundHotSwapSamePort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	nodePort := freePort(t)
	e, err := engine.Start(ctx, engine.NodeConfig{
		Listen: "0.0.0.0", Port: nodePort, StreamType: "tcp", TLSType: "none", AllowPrivate: true,
	}, []engine.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	old := e.Tag()
	newTag := "vless-swap"
	if err := e.AddInbound(ctx, engine.NodeConfig{
		Listen: "0.0.0.0", Port: nodePort, StreamType: "tcp", TLSType: "none", AllowPrivate: true,
	}, []engine.User{{ID: 1, UUID: testUUID}}, newTag); err != nil {
		t.Fatalf("add same-port inbound: %v", err)
	}
	if err := e.RemoveInbound(ctx, old); err != nil {
		t.Fatalf("remove old inbound: %v", err)
	}
	if e.Tag() != newTag {
		t.Fatalf("tag = %s, want %s", e.Tag(), newTag)
	}
	time.Sleep(200 * time.Millisecond)

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: nodePort, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	time.Sleep(100 * time.Millisecond)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "swapped")
	}))
	defer target.Close()
	hc := socksHTTPClient(t, socksPort)
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("after swap: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "swapped" {
		t.Fatalf("body = %q", b)
	}
}

func TestUserSpeedLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host := localIPv4(t)
	nodePort := freePort(t)
	p, panelSrv := newPanel(t, nodePort)
	defer panelSrv.Close()
	u := user(1)
	u.SpeedLimit = 4 // Mbps
	p.SetUsers(u)

	n := node.New(node.Config{
		PanelURL: panelSrv.URL, APIKey: "test-key", NodeID: "7",
		NodeType: "vless", Listen: "0.0.0.0",
	})
	defer n.Close()
	if err := n.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	socksPort := freePort(t)
	client, err := engine.StartClient(ctx, engine.ClientConfig{
		SocksPort: socksPort, Server: host, Port: nodePort, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	time.Sleep(100 * time.Millisecond)

	payload := make([]byte, 512*1024)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer target.Close()

	start := time.Now()
	resp, err := socksHTTPClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if len(body) != len(payload) {
		t.Fatalf("body len = %d", len(body))
	}
	// 4 Mbps ≈ 500 KB/s; 512 KiB should take about 1s. Allow a generous floor
	// to catch a missing limiter without being flaky on fast machines.
	if elapsed < 400*time.Millisecond {
		t.Fatalf("transfer too fast (%v); speed limit not applied", elapsed)
	}
	t.Logf("512 KiB in %v (4 Mbps user limit)", elapsed)
}
