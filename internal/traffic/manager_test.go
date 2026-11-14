package traffic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

type fakeReader struct {
	mu sync.Mutex
	t  map[int64]xray.Traffic
}

func (f *fakeReader) ReadTraffic() map[int64]xray.Traffic {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.t
	f.t = nil
	return out
}

func TestCollectPushAndRetry(t *testing.T) {
	var ok atomic.Bool
	var batches atomic.Int32
	var got []panel.Traffic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batches.Add(1)
		if !ok.Load() {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		var items []panel.Traffic
		_ = json.NewDecoder(r.Body).Decode(&items)
		got = append(got, items...)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pc, err := panel.New(panel.Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd := &fakeReader{t: map[int64]xray.Traffic{1: {Up: 100, Down: 200}, 2: {Up: 300, Down: 400}}}
	m := New(st, rd)
	if n, err := m.Collect(); err != nil || n != 1000 {
		t.Fatalf("collect n=%d err=%v", n, err)
	}
	if b := m.PendingBytes(); b != 1000 {
		t.Fatalf("pending = %d", b)
	}
	if _, err := m.Push(context.Background(), pc, 500, 1<<20); err == nil {
		t.Fatal("expected push failure")
	}
	if b := m.PendingBytes(); b != 1000 {
		t.Fatalf("pending after failure = %d", b)
	}
	ok.Store(true)
	if n, err := m.Push(context.Background(), pc, 500, 1<<20); err != nil || n != 1000 {
		t.Fatalf("push n=%d err=%v", n, err)
	}
	if b := m.PendingBytes(); b != 0 {
		t.Fatalf("pending after ack = %d", b)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("got = %+v", got)
	}
	if batches.Load() != 2 {
		t.Fatalf("batches = %d", batches.Load())
	}
}

func TestBatchSplit(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	pc, _ := panel.New(panel.Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	rd := &fakeReader{t: map[int64]xray.Traffic{1: {Up: 1}, 2: {Up: 1}, 3: {Up: 1}}}
	m := New(st, rd)
	if _, err := m.Collect(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Push(context.Background(), pc, 2, 1<<20); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("batches = %d", n.Load())
	}
}

// TestFlushMemJournalsBacklog guards #4: the in-memory backlog must be
// flushable on its own, before the journal size guard is consulted.
func TestFlushMemJournalsBacklog(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, &fakeReader{})
	m.mem = []store.TrafficDelta{{UserID: 1, Up: 5, Down: 7, CreatedAt: time.Now()}}
	if m.JournalBytes() != 0 {
		t.Fatalf("journal not empty: %d", m.JournalBytes())
	}
	if m.PendingBytes() == 0 {
		t.Fatal("in-memory backlog not counted by PendingBytes")
	}
	if err := m.FlushMem(); err != nil {
		t.Fatal(err)
	}
	if len(m.mem) != 0 {
		t.Fatalf("backlog kept: %+v", m.mem)
	}
	if m.JournalBytes() == 0 {
		t.Fatal("backlog not journaled")
	}
	if m.PendingBytes() != m.JournalBytes() {
		t.Fatalf("pending=%d journal=%d", m.PendingBytes(), m.JournalBytes())
	}
}

// TestPushIgnoresUnjournaledBacklog guards the other half of #4: an in-memory
// backlog must not block pushing the records that are already durable.
func TestPushIgnoresUnjournaledBacklog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pc, err := panel.New(panel.Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := New(st, &fakeReader{})
	m.mem = []store.TrafficDelta{{UserID: 1, Up: 5, CreatedAt: time.Now()}}
	if _, err := m.Push(context.Background(), pc, 0, 0); err != nil {
		t.Fatalf("push blocked by the in-memory backlog: %v", err)
	}
	if len(m.mem) != 1 {
		t.Fatal("push must not consume the unjournaled backlog")
	}
}

// TestPushDrainsAllPages guards #14: a single Push must keep draining the
// journal instead of stopping after the first page.
func TestPushDrainsAllPages(t *testing.T) {
	var batches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batches.Add(1)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pc, err := panel.New(panel.Options{BaseURL: srv.URL, APIKey: "k", NodeID: "1", AllowHTTP: true, MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const records = 5001 // one more than the per-page limit
	deltas := make([]store.TrafficDelta, records)
	for i := range deltas {
		deltas[i] = store.TrafficDelta{UserID: int64(i%100 + 1), Up: 1, CreatedAt: time.Now()}
	}
	if _, err := st.AppendTraffic(deltas); err != nil {
		t.Fatal(err)
	}
	m := New(st, &fakeReader{})
	if _, err := m.Push(context.Background(), pc, 1000, 1<<20); err != nil {
		t.Fatal(err)
	}
	left, err := st.PendingTraffic(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d records left in the journal", len(left))
	}
	if batches.Load() < 2 {
		t.Fatalf("batches = %d, want at least 2", batches.Load())
	}
}
