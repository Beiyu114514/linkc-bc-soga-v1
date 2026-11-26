package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
)

func newMock(t *testing.T) (*mockpanel.Panel, *httptest.Server, *Client, *store.Store) {
	t.Helper()
	p := mockpanel.New(mockpanel.Config{
		APIKey: "key", NodeID: 7, NodeType: "vless",
		Port: 443, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(mockpanel.User{ID: 1, UUID: "701534e1-8540-49f3-87af-71680a1b97ff", TransferEnable: 1 << 40, Enable: true})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := New(Options{
		BaseURL: srv.URL, APIKey: "key", NodeID: "7", NodeType: "vless",
		AllowHTTP: true, MaxAttempts: 1,
	}, st)
	if err != nil {
		t.Fatal(err)
	}
	return p, srv, c, st
}

func TestFetchETagAndPush(t *testing.T) {
	p, _, c, _ := newMock(t)
	ctx := context.Background()
	n, changed, err := c.FetchNode(ctx)
	if err != nil || !changed || n.Config.Port != 443 || n.NodeType != "vless" {
		t.Fatalf("node=%+v changed=%v err=%v", n, changed, err)
	}
	u, changed, err := c.FetchUsers(ctx)
	if err != nil || !changed || len(u) != 1 || u[0].ID != 1 {
		t.Fatalf("users=%+v changed=%v err=%v", u, changed, err)
	}
	if _, changed, err := c.FetchNode(ctx); err != nil || changed {
		t.Fatalf("second node changed=%v err=%v", changed, err)
	}
	if _, changed, err := c.FetchUsers(ctx); err != nil || changed {
		t.Fatalf("second users changed=%v err=%v", changed, err)
	}
	if err := c.PushTraffic(ctx, []Traffic{{ID: 1, U: 10, D: 20}}); err != nil {
		t.Fatal(err)
	}
	if err := c.PushStatus(ctx, Status{CPUPercent: 23.5, MemTotal: 100, MemUsed: 50, Uptime: 10}, "load_fraction"); err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if len(s.Traffic) != 1 || s.Traffic[0].U != 10 || s.Traffic[0].D != 20 {
		t.Fatalf("traffic = %+v", s.Traffic)
	}
	if len(s.InfoLogs) != 1 || s.InfoLogs[0].Load != "0.235000" {
		t.Fatalf("info = %+v", s.InfoLogs)
	}
	if len(s.Online) != 1 || s.Online[0].OnlineUser != 1 {
		t.Fatalf("online = %+v", s.Online)
	}

	p.SetUsers()
	u, changed, err = c.FetchUsers(ctx)
	if err != nil || !changed || len(u) != 0 {
		t.Fatalf("empty users=%+v changed=%v err=%v", u, changed, err)
	}
}

func TestRetryAndErrors(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"basic": map[string]any{}, "config": map[string]any{"port": 443}, "node_type": "vless",
		})
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true,
		MaxAttempts: 3, RetryBase: timeMillisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.FetchNode(context.Background()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("requests = %d", n.Load())
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer bad.Close()
	c2, _ := New(Options{BaseURL: bad.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 3}, nil)
	_, _, err = c2.FetchNode(context.Background())
	if err == nil || !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

// timeMillisecond avoids importing time in the test body above.
const timeMillisecond = 1_000_000

// failingCache simulates a read-only or full state DB.
type failingCache struct{}

func (failingCache) GetETag(string) (store.ETagRecord, bool, error) {
	return store.ETagRecord{}, false, nil
}

func (failingCache) PutETag(string, store.ETagRecord) error {
	return errors.New("database is read-only")
}

// TestCacheWriteFailureKeepsResponse guards #9: a cache write failure must not
// turn a valid panel response into a fetch error (dump-config opens the state
// DB read-only, and a full disk must not stop config updates).
func TestCacheWriteFailureKeepsResponse(t *testing.T) {
	p := mockpanel.New(mockpanel.Config{
		APIKey: "key", NodeID: 7, NodeType: "vless",
		Port: 443, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(mockpanel.User{ID: 1, UUID: "701534e1-8540-49f3-87af-71680a1b97ff", TransferEnable: 1 << 40, Enable: true})
	srv := httptest.NewServer(p)
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, APIKey: "key", NodeID: "7", NodeType: "vless",
		AllowHTTP: true, MaxAttempts: 1}, failingCache{})
	if err != nil {
		t.Fatal(err)
	}
	n, changed, err := c.FetchNode(context.Background())
	if err != nil {
		t.Fatalf("cache write failure must not fail the fetch: %v", err)
	}
	if !changed || n.Config.Port != 443 {
		t.Fatalf("node=%+v changed=%v", n, changed)
	}
}

// TestRetryAfterIsBounded guards #11: the panel controls Retry-After, so a
// huge value must not stall config apply.
func TestRetryAfterIsBounded(t *testing.T) {
	if got := clampRetryAfter(3600 * time.Second); got != maxRetryAfterWait {
		t.Fatalf("clamp(1h) = %v, want %v", got, maxRetryAfterWait)
	}
	if got := clampRetryAfter(5 * time.Second); got != 5*time.Second {
		t.Fatalf("clamp(5s) = %v", got)
	}
	if got := retryAfter("3600"); got != 3600*time.Second {
		t.Fatalf("retryAfter = %v", got)
	}
}

// Test429DoesNotSleepOnLastAttempt guards the second half of #11: the final
// attempt must return immediately instead of sleeping the full Retry-After.
func Test429DoesNotSleepOnLastAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, _, err := c.FetchNode(context.Background()); err == nil {
		t.Fatal("expected a 429 error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("last attempt slept for %v", elapsed)
	}
}

func TestPostJSONRequiresCodeAndHandlesLarge200(t *testing.T) {
	var bodyToSend atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodyToSend.Load().(string)))
	}))
	defer srv.Close()

	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Missing "code" field on 200 JSON must fail.
	bodyToSend.Store(`{"ok":true}`)
	if err := c.PushTraffic(context.Background(), []Traffic{{ID: 1, U: 10, D: 20}}); err == nil {
		t.Fatal("expected error when 200 JSON lacks code field")
	}

	// Non-zero code must fail.
	bodyToSend.Store(`{"code":1,"msg":"bad"}`)
	if err := c.PushTraffic(context.Background(), []Traffic{{ID: 1, U: 10, D: 20}}); err == nil {
		t.Fatal("expected error when 200 JSON has non-zero code")
	}

	// Large (>4 KiB) 200 JSON with code=0 at the end must NOT be truncated.
	largeOK := `{"pad":"` + strings.Repeat("a", 8192) + `","code":0}`
	bodyToSend.Store(largeOK)
	if err := c.PushTraffic(context.Background(), []Traffic{{ID: 1, U: 10, D: 20}}); err != nil {
		t.Fatalf("large 200 JSON with code=0 failed: %v", err)
	}
}

func TestGetRejectsMalformed200JSONBeforeETagCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"bad-etag"`)
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.FetchNode(context.Background()); err == nil {
		t.Fatal("expected error for malformed JSON body")
	}
	if _, ok, err := st.GetETag("/api/v1/node_info?node_id=1"); err != nil || ok {
		t.Fatalf("malformed 200 response must not be cached: ok=%v err=%v", ok, err)
	}
}
