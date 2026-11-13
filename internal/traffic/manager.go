// Package traffic collects per-user counters into a durable journal and
// pushes them to the panel in batches.
package traffic

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// Reader is the Xray traffic source.
type Reader interface {
	ReadTraffic() map[int64]xray.Traffic
}

// Manager owns collection and journal acknowledgement.
type Manager struct {
	store  *store.Store
	reader Reader

	mu      sync.Mutex
	mem     []store.TrafficDelta
	lastErr error
}

func New(st *store.Store, reader Reader) *Manager {
	return &Manager{store: st, reader: reader}
}

// Collect reads and resets counters, then fsyncs them to the journal. If the
// journal write fails the deltas stay in memory and the next Collect refuses
// to read more counters until the backlog is flushed.
func (m *Manager) Collect() (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.mem) > 0 {
		if _, err := m.store.AppendTraffic(m.mem); err != nil {
			m.lastErr = err
			return 0, fmt.Errorf("traffic: flush memory backlog: %w", err)
		}
		m.mem = nil
	}

	raw := m.reader.ReadTraffic()
	if len(raw) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(raw))
	for id, t := range raw {
		if t.Up == 0 && t.Down == 0 {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	deltas := make([]store.TrafficDelta, 0, len(ids))
	var bytes int64
	now := time.Now()
	for _, id := range ids {
		t := raw[id]
		deltas = append(deltas, store.TrafficDelta{UserID: id, Up: t.Up, Down: t.Down, CreatedAt: now})
		bytes += t.Up + t.Down
	}
	if len(deltas) == 0 {
		return 0, nil
	}
	if _, err := m.store.AppendTraffic(deltas); err != nil {
		// Counters have already been reset; keep the data in memory.
		m.mem = append(m.mem, deltas...)
		m.lastErr = err
		return 0, fmt.Errorf("traffic: journal append: %w", err)
	}
	return bytes, nil
}

// Push sends pending journal records. Success is acknowledged record by
// record; a crash between POST and ack can duplicate a batch (at-least-once).
func (m *Manager) Push(ctx context.Context, client *panel.Client, batchUsers int, batchBytes int64) (int64, error) {
	m.mu.Lock()
	mem := append([]store.TrafficDelta(nil), m.mem...)
	m.mu.Unlock()
	if len(mem) > 0 {
		return 0, fmt.Errorf("traffic: %d deltas are not journaled yet", len(mem))
	}
	if batchUsers <= 0 {
		batchUsers = 500
	}
	if batchBytes <= 0 {
		batchBytes = 1 << 20
	}
	const maxRecords = 5000
	records, err := m.store.PendingTraffic(maxRecords)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}

	type agg struct {
		id   int64
		u    int64
		d    int64
		seqs []uint64
	}
	byUser := make(map[int64]*agg)
	order := make([]int64, 0)
	for _, r := range records {
		a := byUser[r.UserID]
		if a == nil {
			a = &agg{id: r.UserID}
			byUser[r.UserID] = a
			order = append(order, r.UserID)
		}
		a.u += r.Up
		a.d += r.Down
		a.seqs = append(a.seqs, r.Seq)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var pushed int64
	var batch []panel.Traffic
	var batchSeqs []uint64
	var batchSize int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := client.PushTraffic(ctx, batch); err != nil {
			m.setErr(err)
			return err
		}
		if err := m.store.AckTraffic(batchSeqs); err != nil {
			m.setErr(err)
			return fmt.Errorf("traffic: ack journal: %w", err)
		}
		for _, t := range batch {
			pushed += t.U + t.D
		}
		batch = nil
		batchSeqs = nil
		batchSize = 0
		return nil
	}
	for _, id := range order {
		a := byUser[id]
		if len(batch) > 0 && (len(batch) >= batchUsers || (batchSize+a.u+a.d > batchBytes && len(batch) > 0)) {
			if err := flush(); err != nil {
				return pushed, err
			}
		}
		batch = append(batch, panel.Traffic{ID: id, U: a.u, D: a.d})
		batchSeqs = append(batchSeqs, a.seqs...)
		batchSize += a.u + a.d
	}
	if err := flush(); err != nil {
		return pushed, err
	}
	return pushed, nil
}

// PendingBytes includes journaled and in-memory (failed journal) bytes.
func (m *Manager) PendingBytes() int64 {
	n, _ := m.store.PendingBytes()
	m.mu.Lock()
	for _, d := range m.mem {
		n += d.Up + d.Down
	}
	m.mu.Unlock()
	return n
}

// LastError returns the most recent journal/push error.
func (m *Manager) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	m.lastErr = err
	m.mu.Unlock()
}
