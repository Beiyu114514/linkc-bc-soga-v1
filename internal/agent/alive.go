package agent

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
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

// pending returns the batches to send without recording them. The snapshot is
// only committed by commit, after the push succeeded, so a failed POST is
// retried on the next tick instead of waiting for the next full snapshot.
func (t *aliveTracker) pending(current map[int64][]string) (batch []panel.AliveIP, full bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	full = t.mode == "full" || (t.pushes+1)%t.fullEach == 0
	for id, ips := range current {
		if len(ips) == 0 {
			continue
		}
		key := ipKey(ips)
		if !full && t.last[id] == key {
			continue
		}
		batch = append(batch, panel.AliveIP{ID: id, IPs: append([]string(nil), ips...)})
	}
	sort.Slice(batch, func(i, j int) bool { return batch[i].ID < batch[j].ID })
	return batch, full
}

// commit records the snapshot that was just delivered.
func (t *aliveTracker) commit(current map[int64][]string, full bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pushes++
	for id, ips := range current {
		if len(ips) == 0 {
			continue
		}
		t.last[id] = ipKey(ips)
	}
	// Drop users that are no longer online from the local cache so their
	// next appearance is sent again.
	if full {
		for id := range t.last {
			if _, ok := current[id]; !ok {
				delete(t.last, id)
			}
		}
	}
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
	a.mu.Unlock()
	return a.pushAliveWithEngine(ctx, engine)
}

func (a *Agent) pushAliveWithEngine(ctx context.Context, engine *xray.Engine) error {
	a.mu.Lock()
	tracker := a.alive
	a.mu.Unlock()
	if engine == nil || tracker == nil {
		return nil
	}
	current := engine.OnlineIPs()
	batch, full := tracker.pending(current)
	if len(batch) == 0 {
		// Nothing to send; still apply the snapshot so offline users are
		// dropped from the cache on a full pass.
		tracker.commit(current, full)
		return nil
	}
	if err := a.panel.PushAliveIP(ctx, batch); err != nil {
		return err
	}
	tracker.commit(current, full)
	return nil
}
