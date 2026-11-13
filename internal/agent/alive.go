package agent

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
)

// aliveTracker sends online IP changes to the panel. The target site only
// upserts rows, so it never clears offline IPs; we therefore only send deltas
// plus a periodic full snapshot.
type aliveTracker struct {
	mu       sync.Mutex
	last     map[int64]string // user -> joined sorted IP list
	pushes   int
	fullEach int
	mode     string
}

func newAliveTracker(fullEach int, mode string) *aliveTracker {
	if fullEach <= 0 {
		fullEach = 20
	}
	if mode == "" {
		mode = "changed"
	}
	return &aliveTracker{last: make(map[int64]string), fullEach: fullEach, mode: mode}
}

// diff returns the batches to send and remembers the new snapshot.
func (t *aliveTracker) diff(current map[int64][]string) []panel.AliveIP {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pushes++
	full := t.mode == "full" || t.pushes%t.fullEach == 0
	var out []panel.AliveIP
	for id, ips := range current {
		if len(ips) == 0 {
			continue
		}
		key := ipKey(ips)
		if !full && t.last[id] == key {
			continue
		}
		t.last[id] = key
		out = append(out, panel.AliveIP{ID: id, IPs: append([]string(nil), ips...)})
	}
	// Drop users that are no longer online from the local cache so their
	// next appearance is sent again.
	for id := range t.last {
		if _, ok := current[id]; !ok && full {
			delete(t.last, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ipKey(ips []string) string {
	cp := append([]string(nil), ips...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// pushAliveIP sends the current online IP snapshot if anything changed.
func (a *Agent) pushAliveIP(ctx context.Context) error {
	a.mu.Lock()
	engine := a.engine
	tracker := a.alive
	a.mu.Unlock()
	if engine == nil || tracker == nil {
		return nil
	}
	batch := tracker.diff(engine.OnlineIPs())
	if len(batch) == 0 {
		return nil
	}
	return a.panel.PushAliveIP(ctx, batch)
}
