package xray

import (
	"context"
	stdnet "net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/time/rate"
)

func gateCtx(email, ip string) context.Context {
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		User:   &protocol.MemoryUser{Email: email},
		Source: net.TCPDestination(net.ParseAddress(ip), 12345),
	})
	return session.ContextWithOutbounds(ctx, []*session.Outbound{{
		Target: net.TCPDestination(net.ParseAddress("example.com"), 443),
	}})
}

func TestDeviceIPv6Aggregation(t *testing.T) {
	g := NewGate(NodeConfig{DeviceIPv6Prefix: 64, LimitsMode: "enforce"}, []User{{ID: 1, DeviceLimit: 1}})
	link := &transport.Link{}
	ctx1 := gateCtx("u1", "2001:db8:1:2::1")
	if !g.Before(ctx1, link) {
		t.Fatal("first IPv6 device rejected")
	}
	// Same /64 must count as the same device.
	link2 := &transport.Link{}
	ctx2 := gateCtx("u1", "2001:db8:1:2::2")
	if !g.Before(ctx2, link2) {
		t.Fatal("same /64 should not be a new device")
	}
	g.After(ctx2, link2)
	// A different /64 must be a second device and be rejected.
	if g.Before(gateCtx("u1", "2001:db8:1:3::1"), &transport.Link{}) {
		t.Fatal("second /64 should be rejected")
	}
	g.After(ctx1, link)
	if !g.Before(gateCtx("u1", "2001:db8:1:3::1"), &transport.Link{}) {
		t.Fatal("device after release should be accepted")
	}
	if got := g.Metrics().RejectsDevice; got != 1 {
		t.Fatalf("device rejects = %d", got)
	}
}

func TestTCPLimitAndReportMode(t *testing.T) {
	g := NewGate(NodeConfig{LimitsMode: "enforce"}, []User{{ID: 1, TCPLimit: 1}})
	link := &transport.Link{}
	ctx := gateCtx("u1", "1.2.3.4")
	if !g.Before(ctx, link) {
		t.Fatal("first TCP rejected")
	}
	if g.Before(gateCtx("u1", "1.2.3.4"), &transport.Link{}) {
		t.Fatal("second TCP should be rejected")
	}
	if got := g.Metrics().RejectsTCP; got != 1 {
		t.Fatalf("tcp rejects = %d", got)
	}
	g.After(ctx, link)

	report := NewGate(NodeConfig{LimitsMode: "report"}, []User{{ID: 1, DeviceLimit: 1}})
	if !report.Before(gateCtx("u1", "1.1.1.1"), &transport.Link{}) ||
		!report.Before(gateCtx("u1", "2.2.2.2"), &transport.Link{}) {
		t.Fatal("report mode must not reject")
	}
	if report.Metrics().RejectsDevice != 0 {
		t.Fatal("report mode must not count rejects")
	}
}

func TestNodeGlobalLimiter(t *testing.T) {
	g := NewGate(NodeConfig{SpeedLimit: 2, LimitsMode: "enforce"}, []User{{ID: 1}})
	link := &transport.Link{}
	if !g.Before(gateCtx("u1", "1.2.3.4"), link) {
		t.Fatal("rejected")
	}
	if g.nodeLimiter == nil {
		t.Fatal("node limiter not installed")
	}
	if link.Reader == nil || link.Writer == nil {
		t.Fatal("node rate wrappers not installed")
	}
	if g.Metrics().NodeSpeed != 2 {
		t.Fatalf("node speed = %v", g.Metrics().NodeSpeed)
	}
}

func TestDeviceKey(t *testing.T) {
	if got := deviceKey("2001:db8::1", 64); got != "2001:db8::/64" {
		t.Fatalf("key = %q", got)
	}
	if got := deviceKey("1.2.3.4", 64); got != "1.2.3.4" {
		t.Fatalf("ipv4 key = %q", got)
	}
}

// TestReportModeTracksDevices guards #15: report mode must still count devices
// so an operator can evaluate a limit before enforcing it.
func TestReportModeTracksDevices(t *testing.T) {
	g := NewGate(NodeConfig{LimitsMode: "report"}, []User{{ID: 1, DeviceLimit: 1}})
	l1 := &transport.Link{}
	l2 := &transport.Link{}
	if !g.Before(gateCtx("u1", "1.1.1.1"), l1) || !g.Before(gateCtx("u1", "2.2.2.2"), l2) {
		t.Fatal("report mode must not reject")
	}
	m := g.Metrics()
	if m.Devices != 2 {
		t.Fatalf("devices = %d, want 2", m.Devices)
	}
	if m.RejectsDevice != 0 {
		t.Fatalf("device rejects = %d, want 0", m.RejectsDevice)
	}
	if _, devices := g.Stats(1); devices != 2 {
		t.Fatalf("Stats devices = %d, want 2", devices)
	}
	g.After(gateCtx("u1", "1.1.1.1"), l1)
	if _, devices := g.Stats(1); devices != 1 {
		t.Fatalf("devices after release = %d, want 1", devices)
	}
}

func TestResolveAndCheckTargetPreservesDomainAndCaches(t *testing.T) {
	g := NewGate(NodeConfig{AllowPrivate: false, LimitsMode: "off"}, nil)
	var lookups atomic.Int32
	gateLookup := make(chan struct{})
	g.lookupIP = func(_ context.Context, _ string, host string) ([]stdnet.IP, error) {
		n := lookups.Add(1)
		if n == 1 {
			<-gateLookup
		}
		switch host {
		case "dual.example":
			// IPv6 + IPv4: must stay Domain so IPv6-only nodes and multi-IP
			// fallback in Freedom are preserved.
			return []stdnet.IP{stdnet.ParseIP("2606:4700:4700::1111"), stdnet.ParseIP("93.184.216.34")}, nil
		case "mixed.example":
			return []stdnet.IP{stdnet.ParseIP("93.184.216.34"), stdnet.ParseIP("127.0.0.1")}, nil
		default:
			return nil, stdnet.ErrClosed
		}
	}

	// Concurrent lookups for the same domain coalesce and cache.
	results := make(chan bool, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
				Target: net.TCPDestination(net.DomainAddress("dual.example"), 443),
			}})
			ok := resolveAndCheckTarget(ctx, g)
			obs := session.OutboundsFromContext(ctx)
			if ok && !obs[0].Target.Address.Family().IsDomain() {
				t.Errorf("target domain was overwritten with IP: %v", obs[0].Target.Address)
			}
			results <- ok
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(gateLookup)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("dual-stack public domain must be allowed")
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1 (singleflight + cache)", got)
	}

	// Repeat after completion must hit cache without another DNS call.
	ctxCached := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress("DUAL.EXAMPLE."), 443),
	}})
	if !resolveAndCheckTarget(ctxCached, g) {
		t.Fatal("cached domain lookup must succeed")
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("lookups after cache hit = %d, want 1", got)
	}

	// Mixed public + private IP must be rejected.
	ctxMixed := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress("mixed.example"), 443),
	}})
	if resolveAndCheckTarget(ctxMixed, g) {
		t.Fatal("mixed public+private domain must be blocked")
	}

	// IP-literal domain string (RFC 6890 / loopback) must be blocked without DNS.
	beforeLiteral := lookups.Load()
	ctxLiteral := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress("[::1]"), 443),
	}})
	if resolveAndCheckTarget(ctxLiteral, g) {
		t.Fatal("loopback IPv6 literal must be blocked")
	}
	if lookups.Load() != beforeLiteral {
		t.Fatal("IP literal should not trigger lookupIP")
	}
}

func TestActiveConnsTracksUDPAndOffMode(t *testing.T) {
	udpCtx := func(email, ip string) context.Context {
		ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
			User:   &protocol.MemoryUser{Email: email},
			Source: net.TCPDestination(net.ParseAddress(ip), 12345),
		})
		return session.ContextWithOutbounds(ctx, []*session.Outbound{{
			Target: net.UDPDestination(net.ParseAddress("8.8.8.8"), 53),
		}})
	}

	enforceGate := NewGate(NodeConfig{LimitsMode: "enforce"}, []User{{ID: 1}})
	udpLink := &transport.Link{}
	if !enforceGate.Before(udpCtx("u1", "1.2.3.4"), udpLink) {
		t.Fatal("udp Before rejected")
	}
	if tcp, _ := enforceGate.Stats(1); tcp != 0 {
		t.Fatalf("UDP activeTCP = %d, want 0", tcp)
	}
	if got := enforceGate.ActiveConns(1); got != 1 {
		t.Fatalf("UDP ActiveConns = %d, want 1", got)
	}
	enforceGate.After(udpCtx("u1", "1.2.3.4"), udpLink)
	if got := enforceGate.ActiveConns(1); got != 0 {
		t.Fatalf("UDP ActiveConns after close = %d, want 0", got)
	}

	offGate := NewGate(NodeConfig{LimitsMode: "off"}, []User{{ID: 1}})
	offLink := &transport.Link{}
	if !offGate.Before(gateCtx("u1", "1.2.3.4"), offLink) {
		t.Fatal("off mode Before rejected")
	}
	if got := offGate.ActiveConns(1); got != 1 {
		t.Fatalf("off mode ActiveConns = %d, want 1", got)
	}
	offGate.After(gateCtx("u1", "1.2.3.4"), offLink)
	if got := offGate.ActiveConns(1); got != 0 {
		t.Fatalf("off mode ActiveConns after close = %d, want 0", got)
	}
}

func TestWaitLimitsSurvivesConcurrentBurstShrink(t *testing.T) {
	lim := rate.NewLimiter(rate.Limit(10_000_000), 128*1024)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		bursts := []int{128 * 1024, 1024, 32 * 1024, 512}
		for i := 0; !stop.Load(); i++ {
			lim.SetLimit(rate.Limit(10_000_000))
			lim.SetBurst(bursts[i%len(bursts)])
		}
	}()

	for i := 0; i < 40; i++ {
		if err := waitLimits(ctx, []*rate.Limiter{lim}, 8192); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("waitLimits failed on concurrent burst shrink: %v", err)
		}
	}
	stop.Store(true)
	wg.Wait()
}
