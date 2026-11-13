package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

func TestBuildConfigTransportMatrix(t *testing.T) {
	base := NodeConfig{Listen: "0.0.0.0", Port: 443, Flow: "xtls-rprx-vision"}
	users := []User{{ID: 1, UUID: "701534e1-8540-49f3-87af-71680a1b97ff"}}

	cases := []struct {
		name    string
		node    NodeConfig
		wantErr string
		check   func(t *testing.T, cfg map[string]any)
	}{
		{
			name: "tcp reality",
			node: func() NodeConfig {
				n := base
				n.StreamType = "tcp"
				n.TLSType = "reality"
				n.ServerNames = []string{"www.example.com"}
				n.PrivateKey = "priv"
				n.ShortIDs = []string{"0123456789abcdef"}
				n.Dest = "www.example.com:443"
				return n
			}(),
			check: func(t *testing.T, cfg map[string]any) {
				in := cfg["inbounds"].([]any)[0].(map[string]any)
				ss := in["streamSettings"].(map[string]any)
				if ss["network"] != "raw" || ss["security"] != "reality" {
					t.Fatalf("stream = %+v", ss)
				}
				if ss["realitySettings"].(map[string]any)["dest"] != "www.example.com:443" {
					t.Fatalf("reality = %+v", ss["realitySettings"])
				}
				settings := in["settings"].(map[string]any)
				if settings["flow"] != "xtls-rprx-vision" {
					t.Fatalf("flow = %v", settings["flow"])
				}
			},
		},
		{
			name: "ws tls",
			node: func() NodeConfig {
				n := base
				n.StreamType = "ws"
				n.TLSType = "tls"
				n.Path = "/ws"
				n.CertFile = "/tmp/cert.pem"
				n.KeyFile = "/tmp/key.pem"
				n.Flow = ""
				return n
			}(),
			check: func(t *testing.T, cfg map[string]any) {
				in := cfg["inbounds"].([]any)[0].(map[string]any)
				ss := in["streamSettings"].(map[string]any)
				if ss["network"] != "ws" || ss["security"] != "tls" {
					t.Fatalf("stream = %+v", ss)
				}
				if ss["wsSettings"].(map[string]any)["path"] != "/ws" {
					t.Fatalf("ws = %+v", ss["wsSettings"])
				}
			},
		},
		{
			name: "xhttp",
			node: func() NodeConfig {
				n := base
				n.StreamType = "xhttp"
				n.TLSType = "reality"
				n.Path = "/x"
				n.Flow = ""
				n.ServerNames = []string{"www.example.com"}
				n.PrivateKey = "priv"
				return n
			}(),
			check: func(t *testing.T, cfg map[string]any) {
				in := cfg["inbounds"].([]any)[0].(map[string]any)
				ss := in["streamSettings"].(map[string]any)
				if ss["network"] != "xhttp" {
					t.Fatalf("stream = %+v", ss)
				}
				if ss["xhttpSettings"].(map[string]any)["path"] != "/x" {
					t.Fatalf("xhttp = %+v", ss["xhttpSettings"])
				}
			},
		},
		{
			name: "h2 removed",
			node: func() NodeConfig {
				n := base
				n.StreamType = "h2"
				n.TLSType = "tls"
				return n
			}(),
			wantErr: "removed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := BuildConfig(tc.node, users, "vless-test")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err := json.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatal(err)
			}
			tc.check(t, cfg)
		})
	}
}

func testCtx(id, ip string) context.Context {
	ctx := session.ContextWithInbound(context.Background(), &session.Inbound{
		User:   &protocol.MemoryUser{Email: id},
		Source: net.TCPDestination(net.ParseAddress(ip), 12345),
	})
	return session.ContextWithOutbounds(ctx, []*session.Outbound{{
		Target: net.TCPDestination(net.ParseAddress("example.com"), 443),
	}})
}

func TestGateTCPAndDeviceLimits(t *testing.T) {
	g := NewGate(NodeConfig{}, []User{
		{ID: 1, TCPLimit: 1, DeviceLimit: 1},
	})
	link := &transport.Link{}
	ctx1 := testCtx("u1", "1.2.3.4")
	if !g.Before(ctx1, link) {
		t.Fatal("first TCP connection rejected")
	}
	// Same user, different source IP: device limit is now exceeded.
	ctx2 := testCtx("u1", "5.6.7.8")
	if g.Before(ctx2, link) {
		t.Fatal("second device should be rejected")
	}
	// Same source IP, second TCP stream: TCP limit is exceeded.
	ctx3 := testCtx("u1", "1.2.3.4")
	if g.Before(ctx3, link) {
		t.Fatal("second TCP stream should be rejected")
	}
	g.After(ctx1, link)
	// Now a new connection is allowed again.
	if !g.Before(testCtx("u1", "5.6.7.8"), link) {
		t.Fatal("connection after release should be accepted")
	}
}

func TestGateSpeedReplacesLink(t *testing.T) {
	g := NewGate(NodeConfig{}, []User{{ID: 1, SpeedLimit: 1}})
	link := &transport.Link{}
	ctx := testCtx("u1", "1.2.3.4")
	if !g.Before(ctx, link) {
		t.Fatal("rejected")
	}
	if link.Reader == nil || link.Writer == nil {
		t.Fatal("rate wrappers not installed")
	}
	g.After(ctx, link)
}
