package xray

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
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
