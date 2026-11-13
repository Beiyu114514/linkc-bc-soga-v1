package agent_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/agent"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
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

func panelUser() mockpanel.User {
	return mockpanel.User{ID: 1, UUID: testUUID, TransferEnable: 1 << 40, Enable: true, ExpireIn: time.Now().Add(time.Hour)}
}

func startPanel(t *testing.T, port int) (*mockpanel.Panel, *httptest.Server) {
	t.Helper()
	p := mockpanel.New(mockpanel.Config{
		APIKey: "test-key", NodeID: 7, NodeType: "vless",
		Port: port, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(panelUser())
	srv := httptest.NewServer(p)
	return p, srv
}

func testConfig(t *testing.T, panelURL, storage string) config.Config {
	t.Helper()
	c := config.Default()
	c.Panel.BaseURL = panelURL
	c.Panel.APIKey = "test-key"
	c.Panel.NodeID = "7"
	c.Panel.NodeType = "vless"
	c.Panel.AllowHTTP = true
	c.Panel.MaxAttempts = 1
	c.Panel.Timeout = config.Duration(2 * time.Second)
	c.Storage.Path = storage
	c.Node.Listen = "127.0.0.1"
	c.Node.PullInterval = config.Duration(50 * time.Millisecond)
	c.Node.PushInterval = config.Duration(50 * time.Millisecond)
	c.Node.CollectInterval = config.Duration(50 * time.Millisecond)
	c.Node.StatusInterval = config.Duration(50 * time.Millisecond)
	c.Node.Freedom.AllowPrivateTargets = true
	c.Node.Users.AllowEmpty = true
	c.Node.Limits.Mode = "off"
	c.Admin.Socket = ""
	return c
}

func startAgent(t *testing.T, cfg config.Config) (*agent.Agent, func(), <-chan error) {
	t.Helper()
	a, err := agent.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				t.Logf("agent did not stop; stacks:\n%s", buf[:n])
				t.Error("agent did not stop")
			}
			_ = a.Close()
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.Snapshot().Ready {
			return a, stop, done
		}
		select {
		case err := <-done:
			t.Fatalf("agent stopped during bootstrap: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("agent did not become ready")
	return nil, nil, nil
}

func socksHTTPClient(t *testing.T, socksPort int) *http.Client {
	t.Helper()
	d, err := xproxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		t.Fatal("socks dialer not context aware")
	}
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: cd.DialContext}}
}

func startVLESSClient(t *testing.T, ctx context.Context, nodePort int) (int, *http.Client) {
	t.Helper()
	return startVLESSClientAt(t, ctx, "127.0.0.1", nodePort)
}

func startVLESSClientAt(t *testing.T, ctx context.Context, server string, nodePort int) (int, *http.Client) {
	t.Helper()
	socksPort := freePort(t)
	c, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: server, Port: nodePort, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return socksPort, socksHTTPClient(t, socksPort)
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestAgentEndToEndAndPanelOutage(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "agent-ok")
	}))
	defer target.Close()
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("client through node: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "agent-ok" {
		t.Fatalf("body = %q", body)
	}

	waitFor(t, "traffic/status in panel", func() bool {
		s := p.Snapshot()
		return len(s.Traffic) > 0 && len(s.Online) > 0 && len(s.InfoLogs) > 0
	})
	s := p.Snapshot()
	if s.Traffic[0].UserID != 1 || s.Traffic[0].U+s.Traffic[0].D == 0 {
		t.Fatalf("traffic = %+v", s.Traffic)
	}
	load, err := strconv.ParseFloat(s.InfoLogs[0].Load, 64)
	if err != nil || load < 0 || load > 1 {
		t.Fatalf("status load = %q", s.InfoLogs[0].Load)
	}

	// Panel outage must not stop the data plane, including new client sessions.
	srv.Close()
	_, hcOutage := startVLESSClient(t, ctx, nodePort)
	resp, err = hcOutage.Get(target.URL)
	if err != nil {
		t.Fatalf("client after panel outage: %v", err)
	}
	resp.Body.Close()
	waitFor(t, "degraded state", func() bool { return a.Snapshot().Degraded })
	if a.Snapshot().LastError == "" {
		t.Fatal("expected last_error during outage")
	}
	stop()

	// The same state DB must boot without a reachable panel.
	cfg2 := cfg
	a2, stop2, _ := startAgent(t, cfg2)
	defer stop2()
	if !a2.Snapshot().Ready {
		t.Fatal("restart from last_good not ready")
	}
	_, hc2 := startVLESSClient(t, ctx, nodePort)
	resp, err = hc2.Get(target.URL)
	if err != nil {
		t.Fatalf("client after last_good restart: %v", err)
	}
	resp.Body.Close()
}

func TestAgentUserHotUpdate(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("initial: %v", err)
	} else {
		resp.Body.Close()
	}

	p.SetUsers()
	waitFor(t, "user removal", func() bool {
		_, err := hc.Get(target.URL)
		return err != nil
	})
	p.SetUsers(panelUser())
	waitFor(t, "user re-add", func() bool {
		resp, err := hc.Get(target.URL)
		if err == nil {
			resp.Body.Close()
			return true
		}
		return false
	})
}

func TestEmptyUsersIsLegal(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	p.SetUsers()
	waitFor(t, "empty users applied", func() bool {
		e := a.Engine()
		return e != nil && len(e.OnlineIPs()) == 0 && a.Snapshot().Users == 0
	})
}

func TestInboundSwapThenUserApply(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("initial: %v", err)
	} else {
		resp.Body.Close()
	}

	newPort := freePort(t)
	pc := p.Snapshot().Config
	pc.Port = newPort
	p.SetNodeConfig(pc)
	waitFor(t, "new inbound port", func() bool { return a.Snapshot().Port == newPort })

	// A user apply must target the surviving inbound, not the removed tag.
	p.SetUsers()
	time.Sleep(200 * time.Millisecond)
	p.SetUsers(panelUser())
	waitFor(t, "user re-add after inbound swap", func() bool {
		_, hc2 := startVLESSClient(t, ctx, newPort)
		resp, err := hc2.Get(target.URL)
		if err == nil {
			resp.Body.Close()
			return true
		}
		return false
	})
}
