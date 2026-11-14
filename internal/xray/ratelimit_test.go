package xray

import (
	"context"
	"testing"

	"golang.org/x/time/rate"
)

// TestWaitLimitsLargeBuffer guards against rate.Limiter.WaitN rejecting a
// buffer larger than the limiter burst, which would break connections.
func TestWaitLimitsLargeBuffer(t *testing.T) {
	lim := rate.NewLimiter(rate.Limit(1<<30), 1024)
	ctx := context.Background()
	if err := waitLimits(ctx, []*rate.Limiter{lim}, 4096); err != nil {
		t.Fatalf("waitLimits(4096) with burst 1024: %v", err)
	}
	// The old code path would have failed; assert the stdlib behavior we guard.
	if err := lim.WaitN(ctx, 4096); err == nil {
		t.Log("note: WaitN accepted oversized request in this Go version")
	}
}
