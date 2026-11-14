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

// TestAuditLastGoodMatchesAppliedRules guards the restart half of #2: the
// update retried on a later pull sees changed=false (the ETag already
// advanced), so last_good must still be written for the version that actually
// became live. Otherwise a restart replays the older last_good and the cached
// newer body is never applied again.
func TestAuditLastGoodMatchesAppliedRules(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"})
	dir := t.TempDir()
	cfg := testConfig(t, srv.URL, dir)
	a, stop, _ := startAgent(t, cfg)
	waitFor(t, "first rule set", func() bool { return a.Snapshot().AuditRules == 1 })

	// New rules arrive but the whitelist fetch fails, so the update is
	// deferred while the audit_rules ETag moves forward.
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"}, mockpanel.Rule{ID: 2, Regex: "keyword:two"})
	p.SetFault("/api/v1/white_list", 500, 1)
	waitFor(t, "audit rule update retried", func() bool { return a.Snapshot().AuditRules == 2 })

	// Restart on the same state DB: the applied set must survive.
	stop()
	b, stopB, _ := startAgent(t, cfg)
	defer stopB()
	if got := b.Snapshot().AuditRules; got != 2 {
		t.Fatalf("after restart audit rules = %d, want 2 (last_good lagged the applied rules)", got)
	}
}

// statusRecorder captures the status code the mock panel replied with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// TestAuditPendingSurvivesRestart guards the durable half of #2: auditPending
// lives in memory, so a restart between the failed pull and its retry loses it.
// The ETag cache already holds the newer body while last_good holds the older
// one, so the first Sync after Bootstrap must still apply the newer set.
func TestAuditPendingSurvivesRestart(t *testing.T) {
	nodePort := freePort(t)
	p := mockpanel.New(mockpanel.Config{
		APIKey: m3TestAPIKey, NodeID: 7, NodeType: "vless",
		Port: nodePort, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(panelUser())
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"})
	var failWhite atomic.Bool
	var rulesNotModified atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failWhite.Load() && r.URL.Path == "/api/v1/white_list" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		p.ServeHTTP(rec, r)
		if r.URL.Path == "/api/v1/audit_rules" && rec.status == http.StatusNotModified {
			rulesNotModified.Add(1)
		}
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	waitFor(t, "first rule set", func() bool { return a.Snapshot().AuditRules == 1 })

	// New rules arrive, but white_list keeps failing so the update can never be
	// applied before the restart. The audit_rules ETag still advances.
	before := rulesNotModified.Load()
	failWhite.Store(true)
	p.SetRules(mockpanel.Rule{ID: 1, Regex: "keyword:one"}, mockpanel.Rule{ID: 2, Regex: "keyword:two"})
	// Two 304s prove the client cached the new body: a 304 for the stale ETag
	// is followed by a 200 (new body), so the second 304 must be for the new one.
	waitFor(t, "new rules cached", func() bool { return rulesNotModified.Load() >= before+2 })
	if got := a.Snapshot().AuditRules; got != 1 {
		t.Fatalf("rules applied while the whitelist fetch fails: %d", got)
	}
	stop()

	// Heal the panel and restart on the same state DB.
	failWhite.Store(false)
	b, stopB, _ := startAgent(t, cfg)
	defer stopB()
	waitFor(t, "cached rule set applied after restart", func() bool { return b.Snapshot().AuditRules == 2 })
}
