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
