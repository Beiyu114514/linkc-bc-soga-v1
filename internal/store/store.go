// Package store persists runtime state in a single bbolt file.
//
// It intentionally contains only the M1 state: ETag caches, last-known-good
// panel objects and the traffic journal. Audit/alive-IP buckets are reserved
// for later milestones.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const schemaVersion = "1"

var (
	bucketMeta        = []byte("meta")
	bucketETag        = []byte("etag")
	bucketLastGood    = []byte("last_good")
	bucketTraffic     = []byte("traffic")
	bucketTrafficMeta = []byte("traffic_meta")
	bucketAudit       = []byte("audit_queue")
	bucketAuditMeta   = []byte("audit_meta")
	bucketAuditDedup  = []byte("audit_dedupe")

	keySchema       = []byte("schema_version")
	keyNextSeq      = []byte("next_seq")
	keyNextAuditSeq = []byte("next_audit_seq")
)

// ErrNotFound is returned when a requested record is absent.
var ErrNotFound = errors.New("store: not found")

// Store owns the bbolt database.
type Store struct {
	mu   sync.Mutex
	db   *bolt.DB
	path string
}

// ETagRecord is an endpoint cache entry.
type ETagRecord struct {
	ETag      string    `json:"etag"`
	Body      []byte    `json:"body"`
	FetchedAt time.Time `json:"fetched_at"`
	SHA256    string    `json:"sha256"`
}

// LastGoodRecord is a persisted panel object.
type LastGoodRecord struct {
	SHA256  string    `json:"sha256"`
	Body    []byte    `json:"body"`
	SavedAt time.Time `json:"saved_at"`
}

// TrafficDelta is one user's traffic since the last collection.
type TrafficDelta struct {
	UserID    int64
	Up        int64
	Down      int64
	CreatedAt time.Time
}

// TrafficRecord is a durable journal entry.
type TrafficRecord struct {
	Seq       uint64    `json:"seq"`
	UserID    int64     `json:"user_id"`
	Up        int64     `json:"up"`
	Down      int64     `json:"down"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditRecord is a durable pending audit event.
type AuditRecord struct {
	Seq       uint64    `json:"seq"`
	UserID    int64     `json:"user_id"`
	RuleID    int64     `json:"rule_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Open opens/creates the state database. Only one process may hold it.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing state database without creating or migrating
// it. It still takes bbolt's shared lock, so a live read-write agent will make
// this time out; callers should treat a failure as "no cache available".
func OpenReadOnly(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 500 * time.Millisecond, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: open read-only %s: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketETag, bucketLastGood, bucketTraffic, bucketTrafficMeta, bucketAudit, bucketAuditMeta, bucketAuditDedup} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		cur := meta.Get(keySchema)
		if cur == nil {
			return meta.Put(keySchema, []byte(schemaVersion))
		}
		if string(cur) != schemaVersion {
			return fmt.Errorf("store: schema %q is not supported (want %s)", cur, schemaVersion)
		}
		return nil
	})
}

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// GetETag loads an endpoint cache entry.
func (s *Store) GetETag(endpoint string) (ETagRecord, bool, error) {
	var rec ETagRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketETag).Get([]byte(endpoint))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// PutETag stores an endpoint cache entry.
func (s *Store) PutETag(endpoint string, rec ETagRecord) error {
	if rec.FetchedAt.IsZero() {
		rec.FetchedAt = time.Now()
	}
	rec.SHA256 = hashBytes(rec.Body)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketETag).Put([]byte(endpoint), b)
	})
}

// GetLastGood decodes a persisted panel object into out.
func (s *Store) GetLastGood(name string, out any) (bool, error) {
	var rec LastGoodRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketLastGood).Get([]byte(name))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil || !found {
		return found, err
	}
	if rec.SHA256 != hashBytes(rec.Body) {
		return false, fmt.Errorf("store: last_good %q checksum mismatch", name)
	}
	if err := json.Unmarshal(rec.Body, out); err != nil {
		return false, fmt.Errorf("store: decode last_good %q: %w", name, err)
	}
	return true, nil
}

// PutLastGood persists a panel object.
func (s *Store) PutLastGood(name string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rec := LastGoodRecord{SHA256: hashBytes(body), Body: body, SavedAt: time.Now()}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketLastGood).Put([]byte(name), b)
	})
}

// AppendTraffic durably appends deltas and returns their sequence numbers.
func (s *Store) AppendTraffic(deltas []TrafficDelta) ([]uint64, error) {
	if len(deltas) == 0 {
		return nil, nil
	}
	seqs := make([]uint64, 0, len(deltas))
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketTrafficMeta)
		next := uint64(1)
		if v := meta.Get(keyNextSeq); len(v) == 8 {
			next = binary.BigEndian.Uint64(v)
			if next == 0 {
				next = 1
			}
		}
		b := tx.Bucket(bucketTraffic)
		for _, d := range deltas {
			if d.Up == 0 && d.Down == 0 {
				continue
			}
			rec := TrafficRecord{
				Seq: next, UserID: d.UserID, Up: d.Up, Down: d.Down,
				CreatedAt: d.CreatedAt,
			}
			if rec.CreatedAt.IsZero() {
				rec.CreatedAt = time.Now()
			}
			body, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], next)
			if err := b.Put(key[:], body); err != nil {
				return err
			}
			seqs = append(seqs, next)
			next++
		}
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], next)
		return meta.Put(keyNextSeq, key[:])
	})
	return seqs, err
}

// PendingTraffic returns up to limit records in sequence order.
func (s *Store) PendingTraffic(limit int) ([]TrafficRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	var out []TrafficRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketTraffic).Cursor()
		for k, v := c.First(); k != nil && len(out) < limit; k, v = c.Next() {
			var rec TrafficRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

// AckTraffic deletes journal records after a successful panel push.
func (s *Store) AckTraffic(seqs []uint64) error {
	if len(seqs) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTraffic)
		for _, seq := range seqs {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], seq)
			if err := b.Delete(key[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingBytes returns the number of unacknowledged traffic bytes.
func (s *Store) PendingBytes() (int64, error) {
	var total int64
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketTraffic).ForEach(func(_, v []byte) error {
			var rec TrafficRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			total += rec.Up + rec.Down
			return nil
		})
	})
	return total, err
}

// PendingCount returns the number of journal records.
func (s *Store) PendingCount() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketTraffic).Stats().KeyN
		return nil
	})
	return n, err
}

// AppendAudit durably enqueues audit events.
func (s *Store) AppendAudit(events []AuditRecord) ([]uint64, error) {
	if len(events) == 0 {
		return nil, nil
	}
	var seqs []uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketAuditMeta)
		next := uint64(1)
		if v := meta.Get(keyNextAuditSeq); len(v) == 8 {
			next = binary.BigEndian.Uint64(v)
			if next == 0 {
				next = 1
			}
		}
		b := tx.Bucket(bucketAudit)
		for _, e := range events {
			e.Seq = next
			if e.CreatedAt.IsZero() {
				e.CreatedAt = time.Now()
			}
			body, err := json.Marshal(e)
			if err != nil {
				return err
			}
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], next)
			if err := b.Put(key[:], body); err != nil {
				return err
			}
			seqs = append(seqs, next)
			next++
		}
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], next)
		return meta.Put(keyNextAuditSeq, key[:])
	})
	return seqs, err
}

// PendingAudit returns up to limit audit events.
func (s *Store) PendingAudit(limit int) ([]AuditRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	var out []AuditRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketAudit).Cursor()
		for k, v := c.First(); k != nil && len(out) < limit; k, v = c.Next() {
			var rec AuditRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

// AckAudit removes audit events after a successful panel push.
func (s *Store) AckAudit(seqs []uint64) error {
	if len(seqs) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		for _, seq := range seqs {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], seq)
			if err := b.Delete(key[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

// AuditDedupeSeen reports whether (user, rule) was reported within ttl.
func (s *Store) AuditDedupeSeen(userID, ruleID int64, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, nil
	}
	key := auditKey(userID, ruleID)
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketAuditDedup).Get([]byte(key))
		if len(v) != 8 {
			return nil
		}
		ts := int64(binary.BigEndian.Uint64(v))
		found = time.Since(time.Unix(ts, 0)) < ttl
		return nil
	})
	return found, err
}

// MarkAuditDedupe records (user, rule) as reported now.
func (s *Store) MarkAuditDedupe(userID, ruleID int64) error {
	key := auditKey(userID, ruleID)
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], uint64(time.Now().Unix()))
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAuditDedup).Put([]byte(key), v[:])
	})
}

// PruneAuditDedupe drops entries older than ttl.
func (s *Store) PruneAuditDedupe(ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-ttl).Unix()
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAuditDedup)
		var del [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			if len(v) == 8 && int64(binary.BigEndian.Uint64(v)) < cutoff {
				del = append(del, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range del {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func auditKey(userID, ruleID int64) string {
	return fmt.Sprintf("%d:%d", userID, ruleID)
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
