package agent

import "testing"

// TestAlivePendingRetriesAfterFailure guards #10: a failed push must not
// advance the snapshot, otherwise the new IP is not resent until the next full
// pass (about 20 minutes by default).
func TestAlivePendingRetriesAfterFailure(t *testing.T) {
	tr := newAliveTracker(20, "changed")
	first := map[int64][]string{42: {"1.1.1.1"}}
	batch, full := tr.pending(first)
	if len(batch) != 1 || full || batch[0].ID != 42 {
		t.Fatalf("pending=%+v full=%v", batch, full)
	}
	// The POST failed: no commit happened, so the same user must be retried.
	batch, _ = tr.pending(first)
	if len(batch) != 1 || batch[0].ID != 42 {
		t.Fatalf("unchanged IP not retried after failure: %+v", batch)
	}
	tr.commit(first, false)
	if batch, _ = tr.pending(first); len(batch) != 0 {
		t.Fatalf("committed snapshot still queued: %+v", batch)
	}
	second := map[int64][]string{42: {"2.2.2.2"}}
	if batch, _ = tr.pending(second); len(batch) != 1 || batch[0].IPs[0] != "2.2.2.2" {
		t.Fatalf("changed IP not detected: %+v", batch)
	}
}

// TestAliveFullPassDropsOffline checks that a committed full pass clears users
// that went offline.
func TestAliveFullPassDropsOffline(t *testing.T) {
	tr := newAliveTracker(1, "changed") // every pass is a full snapshot
	tr.commit(map[int64][]string{42: {"1.1.1.1"}}, true)
	if len(tr.last) != 1 {
		t.Fatalf("last = %+v", tr.last)
	}
	tr.commit(map[int64][]string{}, true)
	if len(tr.last) != 0 {
		t.Fatalf("offline user kept: %+v", tr.last)
	}
}
