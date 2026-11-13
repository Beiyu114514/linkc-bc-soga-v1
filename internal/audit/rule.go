// Package audit compiles soga-v1 detect_list rules into in-memory matchers.
package audit

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
)

// Mode controls how bare (prefix-less) rules are interpreted.
type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeRegex    Mode = "regex"
	ModeKeyword  Mode = "keyword"
	ModePrefixed Mode = "prefixed"
)

// Config controls rule compilation.
type Config struct {
	Mode          string
	Action        string // block | report
	Strict        bool
	MaxRules      int
	DedupeTTL     time.Duration
	GeodataDir    string
	Geo           *GeoData
	ResolveDomain bool
}

// Stats reports compilation results.
type Stats struct {
	Raw      int
	Compiled int
	Skipped  int
	Errors   []string
}

// Rules is an immutable compiled rule set (built once, then swapped).
type Rules struct {
	rules []compiled
	white []compiled
	hasIP bool
	mode  Mode
	cfg   Config

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	id    int64
	white bool
	ok    bool
	at    time.Time
}

type kind int

const (
	kindKeyword kind = iota
	kindRegex
	kindDomain
	kindFull
	kindIP
	kindPort
)

type compiled struct {
	id    int64
	raw   string
	kind  kind
	value string
	re    *regexp.Regexp
	cidrs []*net.IPNet
	ports []portRange
}

type portRange struct{ lo, hi int }

func (c compiled) match(domain string, ip net.IP, port int) bool {
	switch c.kind {
	case kindKeyword:
		return domain != "" && strings.Contains(domain, c.value)
	case kindRegex:
		if domain != "" {
			return c.re.MatchString(domain)
		}
		if ip != nil {
			return c.re.MatchString(ip.String())
		}
		return false
	case kindDomain:
		return domain != "" && (domain == c.value || strings.HasSuffix(domain, "."+c.value))
	case kindFull:
		return domain == c.value
	case kindIP:
		if ip == nil {
			return false
		}
		for _, n := range c.cidrs {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	case kindPort:
		for _, r := range c.ports {
			if port >= r.lo && port <= r.hi {
				return true
			}
		}
		return false
	}
	return false
}

// Compile builds a rule set. Prefixes are always recognized; in auto mode a
// bare value becomes regex only when it contains clear regex syntax.
func Compile(rawRules []panel.AuditRule, white []string, cfg Config) (*Rules, Stats, error) {
	if cfg.MaxRules <= 0 {
		cfg.MaxRules = 5000
	}
	mode := Mode(strings.ToLower(strings.TrimSpace(cfg.Mode)))
	if mode == "" {
		mode = ModeAuto
	}
	switch mode {
	case ModeAuto, ModeRegex, ModeKeyword, ModePrefixed:
	default:
		return nil, Stats{}, fmt.Errorf("audit: unsupported rule_mode %q", cfg.Mode)
	}
	stats := Stats{Raw: len(rawRules)}
	r := &Rules{mode: mode, cfg: cfg}
	seen := make(map[int64]struct{}, len(rawRules))
	for _, raw := range rawRules {
		if raw.ID <= 0 {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("invalid rule id %d", raw.ID))
			continue
		}
		if _, ok := seen[raw.ID]; ok {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("duplicate rule id %d", raw.ID))
			continue
		}
		if len(raw.Rule) == 0 || len(raw.Rule) > 4096 {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("rule %d has invalid length", raw.ID))
			continue
		}
		seen[raw.ID] = struct{}{}
		if p, value, ok := splitPrefix(raw.Rule); ok && (p == "geosite" || p == "geoip") {
			cs, err := cfg.Geo.compileGeo(raw.ID, p, value)
			if err != nil {
				stats.Skipped++
				stats.Errors = append(stats.Errors, fmt.Sprintf("rule %d: %v", raw.ID, err))
				delete(seen, raw.ID)
				continue
			}
			r.rules = append(r.rules, cs...)
			for _, c := range cs {
				if c.kind == kindIP {
					r.hasIP = true
				}
			}
			stats.Compiled++
			continue
		}
		c, err := parseOne(raw.ID, raw.Rule, mode)
		if err != nil {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("rule %d: %v", raw.ID, err))
			delete(seen, raw.ID)
			continue
		}
		r.rules = append(r.rules, c)
		if c.kind == kindIP {
			r.hasIP = true
		}
		stats.Compiled++
		if len(r.rules) >= cfg.MaxRules {
			break
		}
	}
	for _, w := range white {
		if strings.TrimSpace(w) == "" {
			continue
		}
		if p, value, ok := splitPrefix(w); ok && (p == "geosite" || p == "geoip") {
			cs, err := cfg.Geo.compileGeo(0, p, value)
			if err != nil {
				stats.Skipped++
				stats.Errors = append(stats.Errors, "whitelist: "+err.Error())
				continue
			}
			r.white = append(r.white, cs...)
			continue
		}
		c, err := parseOne(0, w, mode)
		if err != nil {
			stats.Skipped++
			stats.Errors = append(stats.Errors, "whitelist: "+err.Error())
			continue
		}
		r.white = append(r.white, c)
	}
	if cfg.Strict && stats.Skipped > 0 {
		return nil, stats, fmt.Errorf("audit: %d rules failed to compile (strict mode)", stats.Skipped)
	}
	return r, stats, nil
}

func parseOne(id int64, raw string, mode Mode) (compiled, error) {
	raw = strings.TrimSpace(raw)
	c := compiled{id: id, raw: raw}
	prefix, value, hasPrefix := splitPrefix(raw)
	if hasPrefix {
		cc, ok, err := parsePrefixed(c, prefix, value)
		if err != nil {
			return compiled{}, err
		}
		if ok {
			return cc, nil
		}
	}
	switch mode {
	case ModePrefixed:
		return compiled{}, fmt.Errorf("rule has no recognized prefix")
	case ModeRegex:
		re, err := regexp.Compile(raw)
		if err != nil {
			return compiled{}, err
		}
		c.kind, c.re = kindRegex, re
		return c, nil
	case ModeKeyword:
		c.kind, c.value = kindKeyword, strings.ToLower(raw)
		return c, nil
	default: // auto
		if looksRegex(raw) {
			re, err := regexp.Compile(raw)
			if err != nil {
				return compiled{}, err
			}
			c.kind, c.re = kindRegex, re
			return c, nil
		}
		c.kind, c.value = kindKeyword, strings.ToLower(raw)
		return c, nil
	}
}

func splitPrefix(raw string) (string, string, bool) {
	i := strings.IndexByte(raw, ':')
	if i <= 0 {
		return "", "", false
	}
	p := strings.ToLower(strings.TrimSpace(raw[:i]))
	switch p {
	case "regexp", "domain", "full", "keyword", "geosite", "ip", "geoip", "port":
		return p, strings.TrimSpace(raw[i+1:]), true
	}
	return "", "", false
}

func parsePrefixed(c compiled, prefix, value string) (compiled, bool, error) {
	switch prefix {
	case "geosite", "geoip":
		return compiled{}, false, fmt.Errorf("geosite/geoip is not enabled in this build")
	case "regexp":
		re, err := regexp.Compile(value)
		if err != nil {
			return compiled{}, true, err
		}
		c.kind, c.re = kindRegex, re
		return c, true, nil
	case "domain":
		c.kind, c.value = kindDomain, strings.ToLower(strings.TrimSuffix(value, "."))
		return c, true, nil
	case "full":
		c.kind, c.value = kindFull, strings.ToLower(strings.TrimSuffix(value, "."))
		return c, true, nil
	case "keyword":
		c.kind, c.value = kindKeyword, strings.ToLower(value)
		return c, true, nil
	case "port":
		ports, err := parsePorts(value)
		if err != nil {
			return compiled{}, true, err
		}
		c.kind, c.ports = kindPort, ports
		return c, true, nil
	case "ip":
		cidrs, err := parseCIDRs(value)
		if err != nil {
			return compiled{}, true, err
		}
		c.kind, c.cidrs = kindIP, cidrs
		return c, true, nil
	}
	return compiled{}, false, nil
}

func parsePorts(value string) ([]portRange, error) {
	var out []portRange
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
				return nil, fmt.Errorf("invalid port range %q", part)
			}
			out = append(out, portRange{lo, hi})
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %q", part)
		}
		out = append(out, portRange{p, p})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty port list")
	}
	return out, nil
}

func parseCIDRs(value string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", part)
			}
			if ip4 := ip.To4(); ip4 != nil {
				part = ip4.String() + "/32"
			} else {
				part = ip.String() + "/128"
			}
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty IP list")
	}
	return out, nil
}

// looksRegex avoids treating ordinary dotted domains as regexes. Only clear
// regex metacharacters trigger the regex path in auto mode.
func regexpCompile(s string) (*regexp.Regexp, error) {
	return regexp.Compile(s)
}

func looksRegex(s string) bool {
	return strings.ContainsAny(s, `\*+?[](){}|^$`)
}

// Match returns the first matching rule ID. Whitelist hits return white=true.
// Rule order follows the panel order.
func (r *Rules) Match(domain string, ip net.IP, port int) (id int64, white bool, ok bool) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	key := domain
	if ip != nil {
		key += "|" + ip.String()
	}
	if key == "" {
		key = "any"
	}
	key += "|" + strconv.Itoa(port)
	if key != "|" {
		r.cacheMu.Lock()
		if e, found := r.cache[key]; found && time.Since(e.at) < 30*time.Second {
			r.cacheMu.Unlock()
			return e.id, e.white, e.ok
		}
		r.cacheMu.Unlock()
	}
	id, white, ok = r.match(domain, ip, port)
	if key != "|" {
		r.cacheMu.Lock()
		if r.cache == nil {
			r.cache = make(map[string]cacheEntry)
		}
		if len(r.cache) > 10000 {
			r.cache = make(map[string]cacheEntry)
		}
		r.cache[key] = cacheEntry{id: id, white: white, ok: ok, at: time.Now()}
		r.cacheMu.Unlock()
	}
	return id, white, ok
}

func (r *Rules) match(domain string, ip net.IP, port int) (id int64, white bool, ok bool) {
	for _, w := range r.white {
		if w.match(domain, ip, port) {
			return 0, true, true
		}
	}
	for _, c := range r.rules {
		if c.match(domain, ip, port) {
			return c.id, false, true
		}
	}
	return 0, false, false
}

// Len returns the number of compiled rules.
func (r *Rules) Len() int { return len(r.rules) }

// HasIPRules reports whether an IP/geoip matcher exists.
func (r *Rules) HasIPRules() bool { return r.hasIP }
