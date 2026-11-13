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
