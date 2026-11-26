package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestETagAndLastGood(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, ok, err := s.GetETag("/node"); err != nil || ok {
		t.Fatalf("empty etag ok=%v err=%v", ok, err)
	}
	rec := ETagRecord{ETag: `"abc"`, Body: []byte(`{"a":1}`), FetchedAt: time.Now()}
	if err := s.PutETag("/node", rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetETag("/node")
	if err != nil || !ok || got.ETag != rec.ETag || string(got.Body) != string(rec.Body) || got.SHA256 == "" {
		t.Fatalf("etag = %+v ok=%v err=%v", got, ok, err)
	}

	type payload struct{ A int }
	if err := s.PutLastGood("node", payload{A: 7}); err != nil {
		t.Fatal(err)
	}
	var p payload
	ok, err = s.GetLastGood("node", &p)
	if err != nil || !ok || p.A != 7 {
		t.Fatalf("last good = %+v ok=%v err=%v", p, ok, err)
	}
}

func TestTrafficJournal(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	seqs, err := s.AppendTraffic([]TrafficDelta{
		{UserID: 1, Up: 10, Down: 20},
		{UserID: 2, Up: 0, Down: 0}, // skipped
		{UserID: 1, Up: 5, Down: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 2 {
		t.Fatalf("seqs = %v", seqs)
	}
	pending, err := s.PendingTraffic(10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if pending[0].UserID != 1 || pending[0].Up != 10 || pending[1].Up != 5 {
		t.Fatalf("pending = %+v", pending)
	}
	b, _ := s.PendingBytes()
	if b != 41 {
		t.Fatalf("pending bytes = %d", b)
	}
	n, _ := s.PendingCount()
	if n != 2 {
		t.Fatalf("pending count = %d", n)
	}
	if err := s.AckTraffic([]uint64{seqs[0]}); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingTraffic(10)
	if len(pending) != 1 || pending[0].Seq != seqs[1] {
		t.Fatalf("pending after ack = %+v", pending)
	}
	if err := s.AckTraffic([]uint64{seqs[1]}); err != nil {
		t.Fatal(err)
	}
	n, _ = s.PendingCount()
	if n != 0 {
		t.Fatalf("pending count after ack = %d", n)
	}
}

func TestReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTraffic([]TrafficDelta{{UserID: 3, Up: 1, Down: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.PendingTraffic(10)
	if err != nil || len(pending) != 1 || pending[0].Up != 1 {
		t.Fatalf("pending after reopen = %+v err=%v", pending, err)
	}
}

func TestOpenReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutLastGood("k", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	var v map[string]int
	ok, err := ro.GetLastGood("k", &v)
	if err != nil || !ok || v["a"] != 1 {
		t.Fatalf("read-only GetLastGood ok=%v v=%v err=%v", ok, v, err)
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("expected error opening missing DB read-only")
	}
}

func TestCloseConcurrentSafety(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.PutLastGood("k", map[string]int{"id": id})
				var out map[string]int
				_, _ = s.GetLastGood("k", &out)
				_, _, _ = s.GetETag("/node")
				_, _ = s.PendingTraffic(10)
				_, _ = s.PendingBytes()
			}
		}(i)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.PutLastGood("k", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("PutLastGood after Close = %v, want ErrClosed", err)
	}
	if _, _, err := s.GetETag("/node"); !errors.Is(err, ErrClosed) {
		t.Fatalf("GetETag after Close = %v, want ErrClosed", err)
	}
	if _, err := s.AppendTraffic([]TrafficDelta{{UserID: 1, Up: 10}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("AppendTraffic after Close = %v, want ErrClosed", err)
	}
}

func TestGetETagDetectsCorruptSHA256(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutETag("/node", ETagRecord{ETag: `"e1"`, Body: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketETag)
		var rec ETagRecord
		if err := json.Unmarshal(b.Get([]byte("/node")), &rec); err != nil {
			return err
		}
		rec.SHA256 = "00000000000000000000000000000000"
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte("/node"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetETag("/node"); err == nil || ok {
		t.Fatalf("corrupt SHA256 must return checksum error and ok=false: ok=%v err=%v", ok, err)
	}
}
