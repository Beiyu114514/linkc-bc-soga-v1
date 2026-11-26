package agent

import (
	"errors"
	"testing"
	"time"
)

func TestAliveTrackerRetriesFullAfterFailure(t *testing.T) {
	tr := newAliveTracker(2, "changed")
	online := map[int64][]string{1: {"192.0.2.1"}}

	// Push #1: incremental, commit succeeds.
	b, full := tr.pending(online)
	if full || len(b) != 1 {
		t.Fatalf("push 1: full=%v len=%d", full, len(b))
	}
	tr.commit(online, full)

	// Push #2: hits fullEach=2 -> full=true, simulate PushAliveIP failure (no commit).
	b, full = tr.pending(online)
	if !full || len(b) != 1 {
		t.Fatalf("push 2: full=%v len=%d", full, len(b))
	}

	// Push #3: must retain full=true until commit() succeeds.
	b, full = tr.pending(online)
	if !full || len(b) != 1 {
		t.Fatalf("push 3 retry must stay full=true, got full=%v len=%d", full, len(b))
	}
	tr.commit(online, full)

	// Push #4: back to incremental until next fullEach.
	b, full = tr.pending(online)
	if full || len(b) != 0 {
		t.Fatalf("push 4: full=%v len=%d", full, len(b))
	}
}

func TestApplyErrorSurvivesEndpointRecovery(t *testing.T) {
	a := &Agent{
		failures: make(map[string]int),
		applyErr: make(map[string]error),
		lastOK:   make(map[string]time.Time),
	}
	a.setApplyError("users", errors.New("apply users failed"))
	a.recordSuccess("node")
	if got := errString(a.lastErr); got != "apply users failed" {
		t.Fatalf("endpoint success cleared apply error: %q", got)
	}
	a.setApplyError("users", nil)
	if got := errString(a.lastErr); got != "" {
		t.Fatalf("clearing apply error left %q", got)
	}
}

func TestAlivePendingRetriesAfterFailure(t *testing.T) {
	tr := newAliveTracker(20, "changed")
	first := map[int64][]string{42: {"1.1.1.1"}}
	batch, full := tr.pending(first)
	if len(batch) != 1 || full || batch[0].ID != 42 {
		t.Fatalf("pending=%+v full=%v", batch, full)
	}
	// Failed POST must not advance the snapshot, so the same user is retried.
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

func TestAliveFullPassDropsOffline(t *testing.T) {
	tr := newAliveTracker(1, "changed")
	tr.commit(map[int64][]string{42: {"1.1.1.1"}}, true)
	if len(tr.last) != 1 {
		t.Fatalf("last = %+v", tr.last)
	}
	tr.commit(map[int64][]string{}, true)
	if len(tr.last) != 0 {
		t.Fatalf("offline user kept: %+v", tr.last)
	}
}
