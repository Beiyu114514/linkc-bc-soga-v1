package store

import (
	"path/filepath"
	"testing"
	"time"
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
