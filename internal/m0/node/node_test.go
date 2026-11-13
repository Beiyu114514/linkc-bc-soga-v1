package node

import "testing"

func TestCollectStatus(t *testing.T) {
	n := New(Config{})
	st := n.CollectStatus()
	if st.MemTotal <= 0 {
		t.Fatalf("mem total = %d", st.MemTotal)
	}
	if st.DiskTotal <= 0 {
		t.Fatalf("disk total = %d", st.DiskTotal)
	}
	if st.Uptime <= 0 {
		t.Fatalf("uptime = %d", st.Uptime)
	}
	if st.CPUPercent < 0 || st.CPUPercent > 100 {
		t.Fatalf("cpu = %v", st.CPUPercent)
	}
}
