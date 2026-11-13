package status

import "testing"

func TestRefresh(t *testing.T) {
	c := New("/")
	s := c.Refresh()
	if s.MemTotal <= 0 || s.DiskTotal <= 0 || s.Uptime <= 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s.CPUPercent < 0 || s.CPUPercent > 100 {
		t.Fatalf("cpu = %v", s.CPUPercent)
	}
	if c.Snapshot() != s {
		t.Fatalf("snapshot mismatch")
	}
}
