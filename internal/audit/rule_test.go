package audit

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	xgeodata "github.com/xtls/xray-core/common/geodata"
	xnet "github.com/xtls/xray-core/common/net"
	"google.golang.org/protobuf/proto"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
)

func mustRules(t *testing.T, rules []panel.AuditRule, white []string, cfg Config) *Rules {
	t.Helper()
	r, _, err := Compile(rules, white, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAutoKeywordsAndRegex(t *testing.T) {
	r := mustRules(t, []panel.AuditRule{
		{ID: 1, Rule: "google"},
		{ID: 2, Rule: `.*facebook\.com`},
		{ID: 3, Rule: "domain:example.com"},
		{ID: 4, Rule: "full:only.example.org"},
	}, nil, Config{Mode: "auto"})
	cases := []struct {
		domain string
		id     int64
		ok     bool
	}{
		{"www.google.com", 1, true},
		{"notgoogle.example", 1, true},
		{"www.facebook.com", 2, true},
		{"a.example.com", 3, true},
		{"example.com", 3, true},
		{"only.example.org", 4, true},
		{"x.only.example.org", 0, false},
		{"other.org", 0, false},
	}
	for _, c := range cases {
		id, white, ok := r.Match(c.domain, nil, 443)
		if white || ok != c.ok || id != c.id {
			t.Fatalf("%s => id=%d white=%v ok=%v, want %d/%v", c.domain, id, white, ok, c.id, c.ok)
		}
	}
}

func TestPortsIPsWhitelist(t *testing.T) {
	r := mustRules(t, []panel.AuditRule{
		{ID: 11, Rule: "ip:10.0.0.0/8,2001:db8::/32"},
		{ID: 10, Rule: "port:8080,8443"},
		{ID: 12, Rule: "domain:blocked.test"},
	}, []string{"domain:allowed.test", "ip:192.0.2.1"}, Config{Mode: "auto"})
	if id, _, ok := r.Match("x.test", nil, 8080); !ok || id != 10 {
		t.Fatalf("port rule: %d %v", id, ok)
	}
	if id, _, ok := r.Match("", net.ParseIP("10.1.2.3"), 443); !ok || id != 11 {
		t.Fatalf("ip rule: %d %v", id, ok)
	}
	if _, _, ok := r.Match("x.test", nil, 443); ok {
		t.Fatal("no rule should match port 443")
	}
	if _, white, ok := r.Match("allowed.test", nil, 443); !ok || !white {
		t.Fatalf("domain whitelist: ok=%v white=%v", ok, white)
	}
	if _, white, ok := r.Match("", net.ParseIP("192.0.2.1"), 443); !ok || !white {
		t.Fatalf("ip whitelist: ok=%v white=%v", ok, white)
	}
	if _, white, ok := r.Match("other.test", nil, 443); ok || white {
		t.Fatalf("unexpected whitelist: ok=%v white=%v", ok, white)
	}
}

func TestModesAndStrict(t *testing.T) {
	if r := mustRules(t, []panel.AuditRule{{ID: 1, Rule: "plain"}}, nil, Config{Mode: "prefixed"}); r.Len() != 0 {
		t.Fatal("prefixed should skip bare rule")
	}
	if r := mustRules(t, []panel.AuditRule{{ID: 1, Rule: `a.*b`}}, nil, Config{Mode: "keyword"}); r.Len() != 1 {
		t.Fatal("keyword rule should compile as a literal substring")
	}
	if _, _, err := Compile([]panel.AuditRule{{ID: 1, Rule: "["}}, nil, Config{Mode: "auto", Strict: true}); err == nil {
		t.Fatal("expected strict regex error")
	}
	if _, stats, err := Compile([]panel.AuditRule{{ID: 1, Rule: "geosite:cn"}}, nil, Config{Mode: "auto"}); err != nil || stats.Skipped == 0 {
		t.Fatalf("geosite should be skipped: stats=%+v err=%v", stats, err)
	}
}

func TestCheckerBlocksAndDedupes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := New(st, Config{Mode: "auto", Action: "block", DedupeTTL: time.Hour}, nil)
	if _, err := c.Update([]panel.AuditRule{{ID: 7, Rule: "blocked.test"}}, nil); err != nil {
		t.Fatal(err)
	}
	dest := xnet.TCPDestination(xnet.DomainAddress("www.blocked.test"), 443)
	if !c.Check(nil, 5, dest) {
		t.Fatal("expected block")
	}
	if !c.Check(nil, 5, dest) {
		t.Fatal("block should be idempotent")
	}
	pending, err := st.PendingAudit(10)
	if err != nil || len(pending) != 1 || pending[0].RuleID != 7 || pending[0].UserID != 5 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if seen, err := st.AuditDedupeSeen(5, 7, time.Hour); err != nil || !seen {
		t.Fatalf("dedupe not marked: seen=%v err=%v", seen, err)
	}
}

func TestCheckerReportAndWhitelist(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := New(st, Config{Mode: "auto", Action: "report", DedupeTTL: time.Hour}, nil)
	if _, err := c.Update([]panel.AuditRule{{ID: 8, Rule: "blocked.test"}}, []string{"full:ok.blocked.test"}); err != nil {
		t.Fatal(err)
	}
	dest := xnet.TCPDestination(xnet.DomainAddress("blocked.test"), 80)
	if c.Check(nil, 1, dest) {
		t.Fatal("report must not block")
	}
	if c.Check(nil, 1, xnet.TCPDestination(xnet.DomainAddress("ok.blocked.test"), 80)) {
		t.Fatal("whitelist must not block")
	}
	pending, _ := st.PendingAudit(10)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestGeoDataRules(t *testing.T) {
	dir := t.TempDir()
	sites, _ := proto.Marshal(&xgeodata.GeoSiteList{Entry: []*xgeodata.GeoSite{{
		Code: "TEST",
		Domain: []*xgeodata.Domain{
			{Type: xgeodata.Domain_Domain, Value: "example.test"},
			{Type: xgeodata.Domain_Full, Value: "full.test"},
		},
	}}})
	ips, _ := proto.Marshal(&xgeodata.GeoIPList{Entry: []*xgeodata.GeoIP{{
		Code: "TESTIP",
		Cidr: []*xgeodata.CIDR{{Ip: []byte{10, 0, 0, 0}, Prefix: 8}},
	}}})
	if err := os.WriteFile(filepath.Join(dir, "geosite.dat"), sites, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geoip.dat"), ips, 0o600); err != nil {
		t.Fatal(err)
	}
	geo, err := LoadGeodata(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, stats, err := Compile([]panel.AuditRule{
		{ID: 1, Rule: "geosite:test"},
		{ID: 2, Rule: "geoip:testip"},
	}, nil, Config{Mode: "auto", Geo: geo})
	if err != nil || stats.Compiled != 2 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if id, _, ok := r.Match("a.example.test", nil, 443); !ok || id != 1 {
		t.Fatalf("geosite domain = %d %v", id, ok)
	}
	if id, _, ok := r.Match("other.test", nil, 443); ok {
		t.Fatalf("geosite false positive: %d", id)
	}
	if id, _, ok := r.Match("", net.ParseIP("10.1.2.3"), 443); !ok || id != 2 {
		t.Fatalf("geoip = %d %v", id, ok)
	}
}

func TestResolveDomainForIPRule(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := New(st, Config{Mode: "auto", Action: "block", DedupeTTL: time.Hour, ResolveDomain: true}, nil)
	if _, err := c.Update([]panel.AuditRule{{ID: 99, Rule: "ip:127.0.0.1/32"}}, nil); err != nil {
		t.Fatal(err)
	}
	dest := xnet.TCPDestination(xnet.DomainAddress("localhost"), 443)
	if !c.Check(nil, 1, dest) {
		t.Fatal("resolved localhost should match ip rule")
	}
}
