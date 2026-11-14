package xray

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
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

// nopWriter accepts and releases everything written to it.
type nopWriter struct{}

func (nopWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return nil
}

// TestRateWriterReleasesBufferOnCancel guards #13: WriteMultiBuffer takes
// ownership of the buffer, so a cancelled wait must not leak it.
func TestRateWriterReleasesBufferOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &rateWriter{
		w:    nopWriter{},
		lims: []*rate.Limiter{rate.NewLimiter(rate.Every(time.Hour), 1)},
		ctx:  ctx,
	}
	b := buf.New()
	b.Write([]byte("payload"))
	if err := w.WriteMultiBuffer(buf.MultiBuffer{b}); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if b.Cap() != 0 {
		t.Fatalf("buffer not released: cap=%d", b.Cap())
	}
}

// TestRateReaderDropsBufferOnCancel guards the reader half of #13: copyInternal
// forwards a non-empty buffer even when the read returned an error, so a
// cancelled read must release it instead of sending one more frame.
func TestRateReaderDropsBufferOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := buf.New()
	b.Write([]byte("payload"))
	r := &rateReader{
		r:    &singleReader{mb: buf.MultiBuffer{b}},
		lims: []*rate.Limiter{rate.NewLimiter(rate.Every(time.Hour), 1)},
		ctx:  ctx,
	}
	mb, err := r.ReadMultiBuffer()
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if !mb.IsEmpty() {
		t.Fatalf("cancelled read returned %d bytes", mb.Len())
	}
	if b.Cap() != 0 {
		t.Fatalf("buffer not released: cap=%d", b.Cap())
	}
}

type singleReader struct {
	mb buf.MultiBuffer
}

func (s *singleReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb := s.mb
	s.mb = nil
	return mb, nil
}

func (s *singleReader) Interrupt() {}
