package audit

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	xnet "github.com/xtls/xray-core/common/net"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
)

// Checker is the Gatekeeper audit hook. Rules can be swapped atomically while
// connections are running.
type Checker struct {
	mu    sync.RWMutex
	rules *Rules
	cfg   Config
	geo   *GeoData
	st    *store.Store
	log   *slog.Logger

	statMu sync.Mutex
	stats  StatCounters

	resolveMu    sync.Mutex
	resolveCache map[string]resolveEntry
}

type resolveEntry struct {
	ips []net.IP
	at  time.Time
}

// StatCounters are runtime audit counters.
type StatCounters struct {
	Hits        uint64
	Whitelisted uint64
	Reported    uint64
	Blocked     uint64
	Errors      uint64
}

func New(st *store.Store, cfg Config, log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Action == "" {
		cfg.Action = "block"
	}
	c := &Checker{cfg: cfg, st: st, log: log}
	c.rules, _, _ = Compile(nil, nil, cfg)
	return c
}

// Update compiles and swaps the rule set. On failure the old set is kept.
func (c *Checker) Update(raw []panel.AuditRule, white []string) (Stats, error) {
	if c.cfg.GeodataDir != "" && c.geo == nil && needsGeodata(raw, white) {
		geo, err := LoadGeodata(c.cfg.GeodataDir)
		if err != nil {
			c.log.Warn("load geodata", "dir", c.cfg.GeodataDir, "error", err)
		}
		c.geo = geo
	}
	cfg := c.cfg
	cfg.Geo = c.geo
	rules, stats, err := Compile(raw, white, cfg)
	if err != nil {
		return stats, err
	}
	c.mu.Lock()
	c.rules = rules
	c.mu.Unlock()
	if stats.Skipped > 0 {
		c.log.Warn("audit rules skipped", "raw", stats.Raw, "compiled", stats.Compiled, "skipped", stats.Skipped)
	}
	return stats, nil
}

// Rules returns the current compiled set.
func (c *Checker) Rules() *Rules {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rules
}

// Check implements the Gatekeeper audit hook. It returns true only when the
// connection must be blocked.
func (c *Checker) Check(ctx context.Context, userID int64, dest xnet.Destination) bool {
	if userID <= 0 || !dest.IsValid() {
		return false
	}
	c.mu.RLock()
	rules := c.rules
	action := strings.ToLower(c.cfg.Action)
	c.mu.RUnlock()
	if rules == nil {
		return false
	}
	domain, ip := target(dest)
	var ruleID int64
	var white, ok bool
	if ip == nil && domain != "" && c.cfg.ResolveDomain && rules.HasIPRules() {
		for _, candidate := range c.resolve(domain) {
			ruleID, white, ok = rules.Match(domain, candidate, int(dest.Port))
			if ok {
				ip = candidate
				break
			}
		}
	} else {
		ruleID, white, ok = rules.Match(domain, ip, int(dest.Port))
	}
	if !ok {
		return false
	}
	c.statMu.Lock()
	c.stats.Hits++
	if white {
		c.stats.Whitelisted++
	}
	c.statMu.Unlock()
	if white {
		return false
	}

	already := false
	if c.st != nil {
		seen, err := c.st.AuditDedupeSeen(userID, ruleID, c.cfg.DedupeTTL)
		if err != nil {
			c.log.Warn("audit dedupe read", "error", err)
		} else {
			already = seen
		}
	}
	if !already {
		if c.st != nil {
			if _, err := c.st.AppendAudit([]store.AuditRecord{{UserID: userID, RuleID: ruleID, CreatedAt: time.Now()}}); err != nil {
				c.log.Warn("audit enqueue", "error", err)
			} else if err := c.st.MarkAuditDedupe(userID, ruleID); err != nil {
				c.log.Warn("audit dedupe write", "error", err)
			}
		}
		c.statMu.Lock()
		c.stats.Reported++
		c.statMu.Unlock()
		c.log.Info("audit hit", "user", userID, "rule", ruleID, "domain", domain, "ip", ipString(ip), "port", dest.Port, "action", action)
	}
	blocked := action == "block"
	if blocked {
		c.statMu.Lock()
		c.stats.Blocked++
		c.statMu.Unlock()
	}
	return blocked
}

// Prune removes expired dedupe keys.
func (c *Checker) Prune() {
	if c.st != nil && c.cfg.DedupeTTL > 0 {
		if err := c.st.PruneAuditDedupe(c.cfg.DedupeTTL); err != nil {
			c.log.Warn("audit dedupe prune", "error", err)
		}
	}
}

// Stats returns a snapshot of runtime counters.
func (c *Checker) Stats() StatCounters {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	return c.stats
}

// target extracts the sniffed domain and/or IP. Sniffing can turn an IP
// literal into a domain (e.g. "127.0.0.1"), so domain values that parse as IPs
// are also exposed to IP rules.
func target(dest xnet.Destination) (string, net.IP) {
	if !dest.Address.Family().IsDomain() {
		return "", dest.Address.IP()
	}
	domain := dest.Address.Domain()
	if ip := net.ParseIP(domain); ip != nil {
		return domain, ip
	}
	return domain, nil
}

func (c *Checker) resolve(domain string) []net.IP {
	c.resolveMu.Lock()
	if e, ok := c.resolveCache[domain]; ok && time.Since(e.at) < time.Minute {
		c.resolveMu.Unlock()
		return e.ips
	}
	c.resolveMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", domain)
	if err != nil || len(ips) == 0 {
		return nil
	}
	c.resolveMu.Lock()
	if c.resolveCache == nil {
		c.resolveCache = make(map[string]resolveEntry)
	}
	if len(c.resolveCache) > 10000 {
		c.resolveCache = make(map[string]resolveEntry)
	}
	c.resolveCache[domain] = resolveEntry{ips: ips, at: time.Now()}
	c.resolveMu.Unlock()
	return ips
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
