package agent_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
)

// The mock panel API key must match testConfig in e2e_test.go.
const m3TestAPIKey = "test-key"

func TestLastErrorClearsAfterRecovery(t *testing.T) {
	nodePort := freePort(t)
	p := mockpanel.New(mockpanel.Config{
		APIKey: m3TestAPIKey, NodeID: 7, NodeType: "vless",
		Port: nodePort, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(panelUser())
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		p.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()

	failing.Store(true)
	waitFor(t, "degraded", func() bool { return a.Snapshot().Degraded })
	if a.Snapshot().LastError == "" {
		t.Fatal("expected last_error during outage")
	}
	failing.Store(false)
	waitFor(t, "last_error cleared", func() bool { return a.Snapshot().LastError == "" })
}

func TestWhitelistFileReloadsWithoutPanelChange(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 77, Regex: "ip:127.0.0.1/32"})

	wl := filepath.Join(t.TempDir(), "wl.txt")
	if err := os.WriteFile(wl, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditConfig(t, srv.URL, t.TempDir())
	cfg.Node.Audit.WhiteListFile = wl
	_, stop, _ := startAgent(t, cfg)
	defer stop()

	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	waitFor(t, "audit rule applied", func() bool { return len(p.Snapshot().Rules) == 1 })
	if resp, err := hc.Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("expected audit block before whitelist")
	}

	// Only the local file changes; the panel keeps returning 304.
	if err := os.WriteFile(wl, []byte("ip:127.0.0.1/32\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "local whitelist reload", func() bool {
		resp, err := hc.Get(target.URL)
		if err == nil {
			resp.Body.Close()
			return true
		}
		return false
	})
}

// TestAuditUpdateRetriedAfterPartialFetchFailure guards #2: when one audit
// endpoint fails, the other one's ETag has already advanced. The next pull
// answers 304 for both, which used to hide the fetched rule version forever.
func TestAuditUpdateRetriedAfterPartialFetchFailure(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"})
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()

	waitFor(t, "first rule set", func() bool { return a.Snapshot().AuditRules == 1 })

	// New rules arrive, but the whitelist fetch fails on the same pull, so the
	// update cannot be applied while the audit_rules ETag moves forward.
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"}, mockpanel.Rule{ID: 2, Regex: "keyword:two"})
	p.SetFault("/api/v1/white_list", 500, 1)

	// The retry pull only sees 304s; the update must still be applied.
	waitFor(t, "audit rule update retried", func() bool { return a.Snapshot().AuditRules == 2 })
}
