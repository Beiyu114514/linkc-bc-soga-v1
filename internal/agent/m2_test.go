package agent_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
)

func auditConfig(t *testing.T, panelURL, storage string) config.Config {
	c := testConfig(t, panelURL, storage)
	c.Node.Audit.Enabled = true
	c.Node.Audit.Action = "block"
	c.Node.Audit.RuleMode = "auto"
	c.Node.Audit.DedupeTTL = config.Duration(24 * time.Hour)
	c.Node.Limits.Mode = "enforce"
	return c
}

func TestAuditBlocksAndReports(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 42, Regex: "ip:127.0.0.1/32"})
	cfg := auditConfig(t, srv.URL, t.TempDir())
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should-not-arrive")
	}))
	defer target.Close()

	waitFor(t, "audit rules applied", func() bool { return len(p.Snapshot().Rules) == 1 })

	// The rule should block the first connection to 127.0.0.1.
	if resp, err := hc.Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("audited connection was not blocked")
	}
	waitFor(t, "audit_log in panel", func() bool { return len(p.Snapshot().Detects) == 1 })
	d := p.Snapshot().Detects[0]
	if d.UserID != 1 || d.ListID != 42 {
		t.Fatalf("detect = %+v", d)
	}
	// A second attempt must not create a duplicate (local dedupe).
	if resp, err := hc.Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("second audited connection was not blocked")
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(p.Snapshot().Detects); n != 1 {
		t.Fatalf("duplicate detect rows: %d", n)
	}
}

func TestAuditReportModeAllows(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 43, Regex: "ip:127.0.0.1/32"})
	cfg := auditConfig(t, srv.URL, t.TempDir())
	cfg.Node.Audit.Action = "report"
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reported")
	}))
	defer target.Close()
	waitFor(t, "audit rules applied", func() bool { return len(p.Snapshot().Rules) == 1 })
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("report mode must allow: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "reported" {
		t.Fatalf("body = %q", body)
	}
	waitFor(t, "audit_log in panel", func() bool { return len(p.Snapshot().Detects) == 1 })
}

func TestWhitelistSkipsAudit(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 44, Regex: "ip:127.0.0.1/32"})
	p.SetWhiteList("ip:127.0.0.1/32")
	cfg := auditConfig(t, srv.URL, t.TempDir())
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "white")
	}))
	defer target.Close()
	waitFor(t, "rules + whitelist", func() bool {
		s := p.Snapshot()
		return len(s.Rules) == 1 && len(s.WhiteList) == 1
	})
	time.Sleep(100 * time.Millisecond) // allow the agent to compile both lists
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("whitelisted connection blocked: %v", err)
	}
	resp.Body.Close()
	time.Sleep(150 * time.Millisecond)
	if n := len(p.Snapshot().Detects); n != 0 {
		t.Fatalf("whitelist should not report, got %d", n)
	}
}

func TestAliveIPPushed(t *testing.T) {
	// Use a non-loopback source address so Xray's OnlineMap records it.
	src := localIPv4(t)
	if src == "127.0.0.1" {
		t.Skip("no non-loopback IPv4 available; Xray ignores loopback in OnlineMap")
	}
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	cfg.Node.Listen = src
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClientAt(t, ctx, src, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, "alive_ip in panel", func() bool {
		for _, a := range p.Snapshot().Alive {
			if a.UserID == 1 && a.IP != "" {
				return true
			}
		}
		return false
	})
}

func localIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var loopback string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			if v4.IsLoopback() {
				loopback = v4.String()
				continue
			}
			// Plain-VLESS test clients require a private non-loopback
			// address. Prefer one over public CI or VPS interfaces.
			if v4.IsPrivate() {
				return v4.String()
			}
		}
	}
	if loopback != "" {
		return loopback
	}
	return "127.0.0.1"
}

func TestAuditDomainBlock(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 45, Text: "blocked.test"})
	cfg := auditConfig(t, srv.URL, t.TempDir())
	cfg.Node.Limits.Mode = "off" // audit must work independently of limits
	_, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "domain-blocked")
	}))
	defer target.Close()
	waitFor(t, "rules", func() bool { return len(p.Snapshot().Rules) == 1 })
	req, err := http.NewRequest(http.MethodGet, target.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "blocked.test"
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("domain rule did not block")
	}
	waitFor(t, "audit_log", func() bool { return len(p.Snapshot().Detects) == 1 })
}

func TestInboundApplyFailureKeepsOld(t *testing.T) {
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
	// tls without a local certificate must be rejected; old inbound must stay.
	bad := p.Snapshot().Config
	bad.TLSType = "tls"
	p.SetNodeConfig(bad)
	time.Sleep(300 * time.Millisecond)
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("old inbound stopped after invalid config: %v", err)
	} else {
		resp.Body.Close()
	}
}
