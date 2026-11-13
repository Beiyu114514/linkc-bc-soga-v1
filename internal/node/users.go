package node

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
	"github.com/google/uuid"
)

// UserOptions controls user-list safety checks.
type UserOptions struct {
	AllowEmpty      bool
	MassRemoveGuard string // warn | reject
	MaxUsers        int
}

// UserStats reports normalization results.
type UserStats struct {
	Raw        int
	Accepted   int
	Invalid    int
	Duplicates int
	Removed    int
	MassRemove bool
}

// NormalizeUsers validates, canonicalizes and de-duplicates panel users.
// It rejects the whole batch only on structural violations (too many invalid
// users, too many users, or a reject-mode mass removal); individual bad rows
// are skipped and reported.
func NormalizeUsers(raw []panel.User, opts UserOptions, previous []xray.User) ([]xray.User, UserStats, error) {
	if opts.MaxUsers <= 0 {
		opts.MaxUsers = 100000
	}
	if opts.MassRemoveGuard == "" {
		opts.MassRemoveGuard = "warn"
	}
	stats := UserStats{Raw: len(raw)}
	if len(raw) == 0 {
		if !opts.AllowEmpty {
			return nil, stats, fmt.Errorf("panel returned an empty user list and users.allow_empty=false")
		}
		return []xray.User{}, stats, nil
	}
	if len(raw) > opts.MaxUsers {
		return nil, stats, fmt.Errorf("user list has %d entries, exceeding max_users=%d", len(raw), opts.MaxUsers)
	}

	byID := make(map[int64]xray.User, len(raw))
	byUUID := make(map[[16]byte]int64, len(raw))
	for _, u := range raw {
		if u.ID <= 0 {
			stats.Invalid++
			continue
		}
		parsed, err := uuid.Parse(strings.TrimSpace(u.UUID))
		if err != nil {
			stats.Invalid++
			continue
		}
		// Xray's VLESS validator zeroes bytes 6/7 before indexing.
		var proc [16]byte
		copy(proc[:], parsed[:])
		proc[6], proc[7] = 0, 0
		if other, ok := byUUID[proc]; ok {
			stats.Duplicates++
			_ = other
			continue
		}
		if _, ok := byID[u.ID]; ok {
			stats.Duplicates++
			continue
		}
		if u.SpeedLimit < 0 || u.DeviceLimit < 0 || u.TCPLimit < 0 {
			stats.Invalid++
			continue
		}
		nu := xray.User{
			ID: u.ID, UUID: parsed.String(),
			SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit, TCPLimit: u.TCPLimit,
		}
		byID[u.ID] = nu
		byUUID[proc] = u.ID
	}

	if stats.Invalid > 0 && float64(stats.Invalid)/float64(len(raw)) > 0.10 {
		return nil, stats, fmt.Errorf("user list has %d/%d invalid entries (>10%%)", stats.Invalid, len(raw))
	}

	prev := make(map[int64]xray.User, len(previous))
	for _, u := range previous {
		prev[u.ID] = u
	}
	removed := 0
	for id := range prev {
		if _, ok := byID[id]; !ok {
			removed++
		}
	}
	stats.Removed = removed
	if len(prev) > 0 && removed > 0 {
		if removed > 1000 || float64(removed)/float64(len(prev)) > 0.5 {
			stats.MassRemove = true
			if opts.MassRemoveGuard == "reject" {
				return nil, stats, fmt.Errorf("mass removal guard: %d/%d users disappeared", removed, len(prev))
			}
		}
	}

	out := make([]xray.User, 0, len(byID))
	for _, u := range byID {
		out = append(out, u)
	}
	SortUsers(out)
	stats.Accepted = len(out)
	return out, stats, nil
}

// UsersFingerprint hashes the fields that affect Xray user registration.
func UsersFingerprint(users []xray.User) string {
	type row struct {
		ID          int64
		UUID        string
		SpeedLimit  float64
		DeviceLimit int
		TCPLimit    int
	}
	rows := make([]row, 0, len(users))
	for _, u := range users {
		rows = append(rows, row{u.ID, u.UUID, u.SpeedLimit, u.DeviceLimit, u.TCPLimit})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%d|%s|%g|%d|%d\n", r.ID, r.UUID, r.SpeedLimit, r.DeviceLimit, r.TCPLimit)
	}
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", h[:])
}
