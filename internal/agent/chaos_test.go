package agent_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// bigTarget serves a payload large enough to move the traffic journal.
func bigTarget(t *testing.T, size int) *httptest.Server {
	t.Helper()
	payload := make([]byte, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPanelGetServerErrorsKeepServing injects 500s on GET /node: the data
// plane must keep serving, the agent must degrade, and it must recover once
// the panel is healthy again.
func TestPanelGetServerErrorsKeepServing(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := bigTarget(t, 4096)

	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("initial request: %v", err)
	} else {
		resp.Body.Close()
	}

	p.SetFault("/api/v1/node", 500, -1)
	waitFor(t, "degraded state", func() bool { return a.Snapshot().Degraded })
	if a.Snapshot().LastError == "" {
		t.Fatal("expected last_error during panel 500s")
	}
	// New client sessions must keep working while the panel is failing.
	_, hc2 := startVLESSClient(t, ctx, nodePort)
	if resp, err := hc2.Get(target.URL); err != nil {
		t.Fatalf("request during panel 500s: %v", err)
	} else {
		resp.Body.Close()
	}

	p.ClearFaults()
	waitFor(t, "recovery", func() bool { return !a.Snapshot().Degraded })
}

// TestPanelWebAPIDisabledKeepsServing covers the panel's own 503 path.
func TestPanelWebAPIDisabledKeepsServing(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := bigTarget(t, 4096)
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("initial request: %v", err)
	} else {
		resp.Body.Close()
	}

	p.SetWebAPIDisabled(true)
	waitFor(t, "degraded state", func() bool { return a.Snapshot().Degraded })
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("request while webapi disabled: %v", err)
	} else {
		resp.Body.Close()
	}

	p.SetWebAPIDisabled(false)
	waitFor(t, "recovery", func() bool { return !a.Snapshot().Degraded })
}

// TestPanelPostThrottleKeepsTrafficJournal injects 429s on POST /traffic:
// the journal must retain the data and flush it once the panel recovers.
func TestPanelPostThrottleKeepsTrafficJournal(t *testing.T) {
	nodePort := freePort(t)
	p, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := bigTarget(t, 64*1024)

	// Drain any startup traffic first.
	waitFor(t, "initial traffic flushed", func() bool { return a.Snapshot().PendingBytes == 0 })

	p.SetFault("/api/v1/traffic", 429, -1)
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("request during throttle: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitFor(t, "traffic journal backlog", func() bool { return a.Snapshot().PendingBytes > 0 })
	backlog := a.Snapshot().PendingBytes
	// The backlog must not grow without bound while POSTs keep failing.
	time.Sleep(300 * time.Millisecond)
	if got := a.Snapshot().PendingBytes; got > backlog*2+1024 {
		t.Fatalf("pending bytes grew from %d to %d while throttled", backlog, got)
	}

	p.ClearFaults()
	waitFor(t, "journal flushed", func() bool { return a.Snapshot().PendingBytes == 0 })
	waitFor(t, "traffic rows in panel", func() bool { return len(p.Snapshot().Traffic) > 0 })
}

// TestTrafficJournalFullPausesCollection verifies the disk-safety valve: when
// the journal exceeds max_journal_bytes, collection pauses instead of growing
// the file without bound, and the node keeps serving.
func TestTrafficJournalFullPausesCollection(t *testing.T) {
	nodePort := freePort(t)
	_, srv := startPanel(t, nodePort)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	cfg.Storage.MaxJournalBytes = 4096
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, nodePort)
	target := bigTarget(t, 32*1024)

	// Take the panel away so the journal cannot drain.
	srv.Close()
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("request with panel down: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, "journal backlog", func() bool { return a.Snapshot().PendingBytes > 0 })
	time.Sleep(400 * time.Millisecond) // let collection hit the cap

	frozen := a.Snapshot().PendingBytes
	if frozen <= 0 {
		t.Fatal("expected a journal backlog")
	}
	// Keep generating traffic; the backlog must stay frozen (collection paused).
	for i := 0; i < 5; i++ {
		resp, err := hc.Get(target.URL)
		if err != nil {
			t.Fatalf("request with full journal: %v", err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond)
	if got := a.Snapshot().PendingBytes; got != frozen {
		t.Fatalf("pending bytes changed while the journal is full: %d -> %d", frozen, got)
	}
	if !a.Snapshot().Ready {
		t.Fatal("agent must stay ready with a full journal")
	}
	// The data plane must keep working for new sessions.
	_, hc2 := startVLESSClient(t, ctx, nodePort)
	if resp, err := hc2.Get(target.URL); err != nil {
		t.Fatalf("new session with full journal: %v", err)
	} else {
		resp.Body.Close()
	}
}
