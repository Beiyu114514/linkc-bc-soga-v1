// Package xray owns the embedded Xray-core instance and the Gatekeeper
// outbound. It is the production runtime derived from the validated M0 spike.
package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/stats"
	confserial "github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/time/rate"
)

// NodeConfig is the validated VLESS inbound configuration.
type NodeConfig struct {
	Listen      string
	Port        int
	StreamType  string
	TLSType     string
	Flow        string
	ServerNames []string
	PrivateKey  string
	ShortIDs    []string
	Dest        string
	Path        string
	ServiceName string
	XVER        int

	SpeedLimit       float64 // node-wide Mbps
	AllowPrivate     bool    // allow private/loopback targets (default false)
	Sniffing         bool    // enable HTTP/TLS/QUIC sniffing
	LimitsMode       string  // enforce | report | off
	BurstBytes       int
	DeviceIPv6Prefix int
	Audit            AuditChecker // optional audit hook; nil disables audit

	// TLS material is local; the panel does not send certificate paths.
	CertFile string
	KeyFile  string
	// Inline PEM avoids Xray's file hot-reload ticker (which has an upstream race).
	CertPEM       string
	KeyPEM        string
	Certificates  []TLSCertificate
	ALPN          []string
	MinVersion    string
	ECHServerKeys string
	Decryption    string
	XHTTP         *XHTTPConfig
	GRPC          *GRPCConfig

	// Client-side fields are used by E2E tests only.
	PinnedPeerCertSha256 string
	ServerName           string
}

// TLSCertificate is one inline server certificate/key pair.
type TLSCertificate struct {
	CertPEM string
	KeyPEM  string
}

// XHTTPConfig is the server-side subset of xhttpSettings.
type XHTTPConfig struct {
	Host    string
	Path    string
	Mode    string
	Headers map[string]string
}

// GRPCConfig is the server-side subset of grpcSettings.
type GRPCConfig struct {
	Authority           string
	ServiceName         string
	MultiMode           bool
	IdleTimeout         int32
	HealthCheckTimeout  int32
	PermitWithoutStream bool
	InitialWindowsSize  int32
	UserAgent           string
}

// User is the subset of GET /api/v1/users the spike needs.
type User struct {
	ID          int64
	UUID        string
	SpeedLimit  float64
	DeviceLimit int
	TCPLimit    int
}

// Traffic is a per-user delta since the last ReadTraffic.
type Traffic struct {
	Up   int64
	Down int64
}

// Engine owns one embedded Xray instance.
type Engine struct {
	mu         sync.Mutex
	inst       *core.Instance
	tag        string
	nodeCfg    NodeConfig
	flow       string
	port       int
	users      map[int64]User
	gate       *Gate
	inboundMgr inbound.Manager
}

// Start starts an embedded Xray instance with one VLESS inbound.
func Start(ctx context.Context, node NodeConfig, users []User) (*Engine, error) {
	if node.Listen == "" {
		node.Listen = "127.0.0.1"
	}
	if err := ValidateConfig(node); err != nil {
		return nil, err
	}
	if err := ValidateUsers(users); err != nil {
		return nil, err
	}
	tag := "vless-1"
	if node.Port == 0 {
		return nil, fmt.Errorf("node port is required")
	}
	cfgJSON, err := BuildConfig(node, users, tag)
	if err != nil {
		return nil, err
	}
	cfg, err := confserial.LoadJSONConfig(strings.NewReader(cfgJSON))
	if err != nil {
		return nil, fmt.Errorf("parse xray config: %w", err)
	}
	inst, err := core.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create xray instance: %w", err)
	}
	if err := inst.Start(); err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("start xray: %w", err)
	}

	ihm := inst.GetFeature(inbound.ManagerType()).(inbound.Manager)
	gate := NewGate(node, users)
	ohm := inst.GetFeature(outbound.ManagerType()).(outbound.Manager)
	inner := ohm.GetHandler("direct")
	if inner == nil {
		_ = inst.Close()
		return nil, fmt.Errorf("direct outbound not found")
	}
	if err := ohm.RemoveHandler(ctx, "direct"); err != nil {
		_ = inst.Close()
		return nil, err
	}
	wrapped := &wrapHandler{Handler: inner, gate: gate}
	if err := ohm.AddHandler(ctx, wrapped); err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("inject gatekeeper: %w", err)
	}

	e := &Engine{
		inst:       inst,
		tag:        tag,
		nodeCfg:    node,
		flow:       node.Flow,
		port:       node.Port,
		users:      make(map[int64]User, len(users)),
		gate:       gate,
		inboundMgr: ihm,
	}
	for _, u := range users {
		e.users[u.ID] = u
	}
	return e, nil
}

// Close stops the instance.
func (e *Engine) Close() error {
	if e.inst == nil {
		return nil
	}
	return e.inst.Close()
}

// Tag returns the current inbound tag.
func (e *Engine) Tag() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tag
}

// NodePort returns the listening port (from the current node config, if known).
func (e *Engine) NodePort() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.port
}

// ApplyUsers reconciles the Xray user set and Gatekeeper limits.
func (e *Engine) ApplyUsers(ctx context.Context, users []User) error {
	if err := ValidateUsers(users); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	um, err := e.userManagerLocked()
	if err != nil {
		return err
	}
	next := make(map[int64]User, len(users))
	for _, u := range users {
		next[u.ID] = u
	}

	// Remove users whose UUID changed or that disappeared.
	for id, old := range e.users {
		nu, ok := next[id]
		if !ok || nu.UUID != old.UUID {
			if err := um.RemoveUser(ctx, email(id)); err != nil {
				// not found is fine on resync
				if !strings.Contains(strings.ToLower(err.Error()), "not found") {
					return err
				}
			}
		}
	}
	// Add new/changed users.
	for id, nu := range next {
		old, ok := e.users[id]
		if ok && old.UUID == nu.UUID {
			continue
		}
		mu, err := memoryUser(nu, e.flow)
		if err != nil {
			return fmt.Errorf("build user %d: %w", id, err)
		}
		if err := um.AddUser(ctx, mu); err != nil {
			// A previous partial apply may already have added this email.
			if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
				return fmt.Errorf("add user %d: %w", id, err)
			}
		}
	}
	e.users = next
	e.gate.UpdateLimits(next)
	return nil
}

// SwapReport describes the outcome of an inbound swap.
type SwapReport struct {
	// Tag is the inbound tag now serving traffic ("" only if the engine
	// ended up with no listener at all).
	Tag string
	// RolledBack is true when the previous inbound was restored because the
	// old listener could not be removed.
	RolledBack bool
	// Warning carries a non-fatal anomaly, e.g. a stale old listener that is
	// still bound to the port.
	Warning string
}

// addInbound is indirected so tests can force add failures. Xray resolves the
// inbound manager from the instance itself (core.AddInboundHandler), so
// wrapping Engine.inboundMgr cannot intercept adds.
var addInbound = func(e *Engine, ctx context.Context, node NodeConfig, users []User, tag string) error {
	return e.addInboundHandler(ctx, node, users, tag)
}

// SwapInbound replaces the live inbound as a transaction:
//
//  1. add the new inbound (same-port overlap relies on SO_REUSEPORT);
//  2. on add failure the previous inbound keeps serving and the error is
//     returned with Tag set to the still-active tag;
//  3. on remove failure of the old listener, roll back to the previous
//     config: remove the new inbound and re-add the old one;
//  4. if the rollback itself fails, the new inbound (which is live and
//     serving) is kept and the error explains the stale state.
//
// After SwapInbound returns, the engine state (tag, users, gate limits)
// always matches the inbound that is actually serving.
func (e *Engine) SwapInbound(ctx context.Context, node NodeConfig, users []User) (SwapReport, error) {
	if node.Listen == "" {
		node.Listen = "127.0.0.1"
	}
	if err := ValidateConfig(node); err != nil {
		return SwapReport{}, err
	}
	if err := ValidateUsers(users); err != nil {
		return SwapReport{}, err
	}
	e.mu.Lock()
	oldTag, oldCfg := e.tag, e.nodeCfg
	oldUsers := e.userListLocked()
	e.mu.Unlock()

	tag := fmt.Sprintf("vless-%d", time.Now().UnixNano())
	if err := addInbound(e, ctx, node, users, tag); err != nil {
		// Nothing changed: the previous inbound keeps serving.
		return SwapReport{Tag: oldTag}, fmt.Errorf("add inbound: %w", err)
	}
	if oldTag == "" {
		// No previous inbound to remove (e.g. recovering from a failed
		// rollback that left the engine without a handler).
		e.commit(node, users, tag)
		return SwapReport{Tag: tag}, nil
	}
	rmErr := e.inboundMgr.RemoveHandler(ctx, oldTag)
	if rmErr == nil {
		e.commit(node, users, tag)
		return SwapReport{Tag: tag}, nil
	}

	// Removing the old listener failed. Roll back to the previous config so
	// a stale inbound cannot keep serving an outdated user set.
	if err := e.inboundMgr.RemoveHandler(ctx, tag); err != nil {
		// The new inbound is live but cannot be removed. Keep serving with
		// it; the stale old listener is reported as a warning.
		e.commit(node, users, tag)
		return SwapReport{Tag: tag, Warning: fmt.Sprintf("old inbound %s still bound: %v", oldTag, rmErr)},
			fmt.Errorf("swap inbound: remove old %s: %w", oldTag, rmErr)
	}
	// A failed remove usually leaves the old handler registered (Xray only
	// logs close errors), so it is already serving again after the new
	// inbound was dropped; only re-add when it is really gone.
	if _, err := e.inboundMgr.GetHandler(ctx, oldTag); err != nil {
		if err := addInbound(e, ctx, oldCfg, oldUsers, oldTag); err != nil {
			// Worst case: no inbound is registered. Try to bring the new
			// config back so the node keeps serving traffic.
			err2 := addInbound(e, ctx, node, users, tag)
			if err2 == nil {
				e.commit(node, users, tag)
				return SwapReport{Tag: tag, Warning: "rollback failed; new inbound restored"},
					fmt.Errorf("swap inbound: remove old %s: %w", oldTag, rmErr)
			}
			e.mu.Lock()
			e.tag = ""
			e.mu.Unlock()
			return SwapReport{}, fmt.Errorf("swap inbound: remove old %s: %w; rollback to old failed: %v; restore new failed: %v",
				oldTag, rmErr, err, err2)
		}
	}
	e.commit(oldCfg, oldUsers, oldTag)
	return SwapReport{Tag: oldTag, RolledBack: true},
		fmt.Errorf("swap inbound: remove old %s: %w; rolled back to previous inbound", oldTag, rmErr)
}

// AddInbound adds a new VLESS inbound (same or different port), then returns
// so the caller can remove the old tag. Same-port overlap relies on Xray's
// SO_REUSEPORT listener. Prefer SwapInbound, which is transactional.
func (e *Engine) AddInbound(ctx context.Context, node NodeConfig, users []User, tag string) error {
	if err := e.addInboundHandler(ctx, node, users, tag); err != nil {
		return err
	}
	e.commit(node, users, tag)
	return nil
}

func (e *Engine) addInboundHandler(ctx context.Context, node NodeConfig, users []User, tag string) error {
	if node.Listen == "" {
		node.Listen = "127.0.0.1"
	}
	if err := ValidateConfig(node); err != nil {
		return err
	}
	if err := ValidateUsers(users); err != nil {
		return err
	}
	cfgJSON, err := BuildConfig(node, users, tag)
	if err != nil {
		return err
	}
	cfg, err := confserial.LoadJSONConfig(strings.NewReader(cfgJSON))
	if err != nil {
		return err
	}
	if len(cfg.Inbound) != 1 {
		return fmt.Errorf("expected one inbound, got %d", len(cfg.Inbound))
	}
	if err := core.AddInboundHandler(e.inst, cfg.Inbound[0]); err != nil {
		return fmt.Errorf("add inbound %s: %w", tag, err)
	}
	return nil
}

// commit makes the engine state match the inbound that is now serving.
func (e *Engine) commit(node NodeConfig, users []User, tag string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tag = tag
	e.flow = node.Flow
	e.port = node.Port
	e.nodeCfg = node
	e.users = make(map[int64]User, len(users))
	for _, u := range users {
		e.users[u.ID] = u
	}
	e.gate.UpdateLimitsMap(users)
}

func (e *Engine) userListLocked() []User {
	out := make([]User, 0, len(e.users))
	for _, u := range e.users {
		out = append(out, u)
	}
	return out
}

// RemoveInbound removes an inbound by tag. Existing connections are left to
// Xray; this spike only verifies listener replacement.
func (e *Engine) RemoveInbound(ctx context.Context, tag string) error {
	return e.inboundMgr.RemoveHandler(ctx, tag)
}

// ReadTraffic atomically reads and resets all user traffic counters.
func (e *Engine) ReadTraffic() map[int64]Traffic {
	sm := e.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	out := make(map[int64]Traffic)
	e.mu.Lock()
	users := make([]User, 0, len(e.users))
	for _, u := range e.users {
		users = append(users, u)
	}
	e.mu.Unlock()
	for _, u := range users {
		var t Traffic
		if c := sm.GetCounter(counterName(u.ID, "uplink")); c != nil {
			t.Up = c.Set(0)
		}
		if c := sm.GetCounter(counterName(u.ID, "downlink")); c != nil {
			t.Down = c.Set(0)
		}
		if t.Up != 0 || t.Down != 0 {
			out[u.ID] = t
		}
	}
	return out
}

// OnlineIPs returns the current Xray OnlineMap contents per user.
func (e *Engine) OnlineIPs() map[int64][]string {
	sm := e.inst.GetFeature(stats.ManagerType()).(stats.Manager)
	out := make(map[int64][]string)
	e.mu.Lock()
	ids := make([]int64, 0, len(e.users))
	for id := range e.users {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		om := sm.GetOnlineMap("user>>>" + email(id) + ">>>online")
		if om == nil {
			continue
		}
		var ips []string
		om.ForEach(func(ip string, _ int64) bool {
			ips = append(ips, ip)
			return true
		})
		if len(ips) > 0 {
			out[id] = ips
		}
	}
	return out
}

// Gate exposes the gatekeeper for tests/admin.
func (e *Engine) Gate() *Gate { return e.gate }

// SetNodeSpeed updates the node-wide rate limiter.
func (e *Engine) SetNodeSpeed(mbps float64) { e.gate.SetNodeSpeed(mbps) }

func (e *Engine) userManagerLocked() (proxy.UserManager, error) {
	h, err := e.inboundMgr.GetHandler(context.Background(), e.tag)
	if err != nil {
		return nil, err
	}
	gi, ok := h.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("inbound is not proxy.GetInbound")
	}
	um, ok := gi.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("inbound is not proxy.UserManager")
	}
	return um, nil
}

// --- config building -------------------------------------------------------

// BuildConfig produces the Xray JSON used by the spike. It is deliberately
// readable so tests can assert the site -> Xray mapping.
func BuildConfig(node NodeConfig, users []User, tag string) (string, error) {
	if err := ValidateConfig(node); err != nil {
		return "", err
	}
	if err := ValidateUsers(users); err != nil {
		return "", err
	}
	if node.StreamType == "" {
		node.StreamType = "tcp"
	}
	if node.TLSType == "" {
		node.TLSType = "none"
	}
	clients := make([]map[string]any, 0, len(users))
	for _, u := range users {
		client := map[string]any{
			"id":    u.UUID,
			"email": email(u.ID),
			"level": 0,
		}
		if node.Flow != "" {
			client["flow"] = node.Flow
		}
		clients = append(clients, client)
	}

	decryption := strings.TrimSpace(node.Decryption)
	if decryption == "" {
		decryption = "none"
	}
	settings := map[string]any{
		"decryption": decryption,
		"clients":    clients,
	}
	if node.Flow != "" {
		settings["flow"] = node.Flow
	}

	stream, err := buildStream(node)
	if err != nil {
		return "", err
	}
	inbound := map[string]any{
		"tag":            tag,
		"listen":         node.Listen,
		"port":           node.Port,
		"protocol":       "vless",
		"settings":       settings,
		"streamSettings": stream,
		"sniffing": map[string]any{
			"enabled":      node.Sniffing,
			"destOverride": []string{"http", "tls", "quic"},
			"routeOnly":    false,
		},
	}

	cfg := map[string]any{
		"log": map[string]any{
			"loglevel": "warning",
			"access":   "none",
		},
		"stats": map[string]any{},
		"policy": map[string]any{
			"levels": map[string]any{
				"0": map[string]any{
					"handshake":         10,
					"connIdle":          180,
					"statsUserUplink":   true,
					"statsUserDownlink": true,
					"statsUserOnline":   true,
				},
			},
		},
		"inbounds": []any{inbound},
		"outbounds": []any{
			map[string]any{
				"tag":      "direct",
				"protocol": "freedom",
				"settings": freedomSettings(node.AllowPrivate),
			},
			map[string]any{"tag": "block", "protocol": "blackhole"},
		},
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{"type": "field", "outboundTag": "direct", "network": "tcp,udp"},
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func freedomSettings(allowPrivate bool) map[string]any {
	settings := map[string]any{}
	if allowPrivate {
		settings["finalRules"] = []any{map[string]any{
			"action":  "allow",
			"network": "tcp,udp",
			"ip": []string{
				"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
				"127.0.0.0/8", "169.254.0.0/16",
				"::1/128", "fc00::/7", "fe80::/10",
			},
		}}
	}
	return settings
}

func buildStream(node NodeConfig) (map[string]any, error) {
	network := strings.ToLower(node.StreamType)
	switch network {
	case "tcp", "raw":
		network = "raw"
	case "ws":
		network = "ws"
	case "grpc":
		network = "grpc"
	case "xhttp":
		network = "xhttp"
	case "http", "h2":
		return nil, fmt.Errorf("transport %q is removed in modern Xray; use xhttp", node.StreamType)
	default:
		return nil, fmt.Errorf("unsupported stream_type %q", node.StreamType)
	}
	stream := map[string]any{
		"network":  network,
		"security": "none",
	}
	switch network {
	case "ws":
		stream["wsSettings"] = map[string]any{"path": node.Path}
	case "grpc":
		gs := map[string]any{}
		svc := node.ServiceName
		if node.GRPC != nil {
			if node.GRPC.ServiceName != "" {
				svc = node.GRPC.ServiceName
			}
			if node.GRPC.Authority != "" {
				gs["authority"] = node.GRPC.Authority
			}
			if node.GRPC.MultiMode {
				gs["multiMode"] = true
			}
			if node.GRPC.IdleTimeout > 0 {
				gs["idle_timeout"] = node.GRPC.IdleTimeout
			}
			if node.GRPC.HealthCheckTimeout > 0 {
				gs["health_check_timeout"] = node.GRPC.HealthCheckTimeout
			}
			if node.GRPC.PermitWithoutStream {
				gs["permit_without_stream"] = true
			}
			if node.GRPC.InitialWindowsSize > 0 {
				gs["initial_windows_size"] = node.GRPC.InitialWindowsSize
			}
			if node.GRPC.UserAgent != "" {
				gs["user_agent"] = node.GRPC.UserAgent
			}
		}
		if svc != "" {
			gs["serviceName"] = svc
		}
		stream["grpcSettings"] = gs
	case "xhttp":
		xs := map[string]any{"mode": "auto"}
		path := node.Path
		if node.XHTTP != nil {
			if node.XHTTP.Path != "" {
				path = node.XHTTP.Path
			}
			if node.XHTTP.Mode != "" {
				xs["mode"] = node.XHTTP.Mode
			}
			if node.XHTTP.Host != "" {
				xs["host"] = node.XHTTP.Host
			}
			if len(node.XHTTP.Headers) > 0 {
				xs["headers"] = node.XHTTP.Headers
			}
		}
		if path != "" {
			xs["path"] = path
		}
		stream["xhttpSettings"] = xs
	}

	switch strings.ToLower(node.TLSType) {
	case "", "none":
	case "tls":
		stream["security"] = "tls"
		tls := map[string]any{}
		certs := node.Certificates
		if len(certs) == 0 && node.CertPEM != "" && node.KeyPEM != "" {
			certs = []TLSCertificate{{CertPEM: node.CertPEM, KeyPEM: node.KeyPEM}}
		}
		switch {
		case len(certs) > 0:
			arr := make([]any, 0, len(certs))
			for _, c := range certs {
				arr = append(arr, map[string]any{
					"certificate": []string{c.CertPEM},
					"key":         []string{c.KeyPEM},
				})
			}
			tls["certificates"] = arr
		case node.CertFile != "" && node.KeyFile != "":
			tls["certificates"] = []any{map[string]any{
				"certificateFile": node.CertFile,
				"keyFile":         node.KeyFile,
			}}
		}
		if len(node.ALPN) > 0 {
			tls["alpn"] = node.ALPN
		}
		if node.ECHServerKeys != "" {
			tls["echServerKeys"] = node.ECHServerKeys
		}
		if node.MinVersion != "" {
			tls["minVersion"] = node.MinVersion
		}
		if node.ServerName != "" {
			tls["serverName"] = node.ServerName
		}
		if node.PinnedPeerCertSha256 != "" {
			tls["pinnedPeerCertSha256"] = node.PinnedPeerCertSha256
		}
		stream["tlsSettings"] = tls
	case "reality":
		if network != "raw" && network != "grpc" && network != "xhttp" {
			return nil, fmt.Errorf("REALITY does not support transport %q", network)
		}
		stream["security"] = "reality"
		reality := map[string]any{
			"dest":        node.Dest,
			"serverNames": node.ServerNames,
			"privateKey":  node.PrivateKey,
			"shortIds":    node.ShortIDs,
			"xver":        node.XVER,
		}
		stream["realitySettings"] = reality
	default:
		return nil, fmt.Errorf("unsupported tls_type %q", node.TLSType)
	}
	return stream, nil
}

func email(id int64) string { return "u" + strconv.FormatInt(id, 10) }

func counterName(id int64, dir string) string {
	return "user>>>" + email(id) + ">>>traffic>>>" + dir
}

func memoryUser(u User, flow string) (*protocol.MemoryUser, error) {
	id, err := parseUUID(u.UUID)
	if err != nil {
		return nil, fmt.Errorf("uuid %q: %w", u.UUID, err)
	}
	return &protocol.MemoryUser{
		Email: email(u.ID),
		Level: 0,
		Account: &vless.MemoryAccount{
			ID:   protocol.NewID(id),
			Flow: flow,
		},
	}, nil
}

func parseUUID(s string) ([16]byte, error) {
	// Keep the spike dependency-free by implementing the tiny canonical parse
	// needed for tests; Xray itself validates the UUID when the inbound starts.
	var out [16]byte
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return out, fmt.Errorf("bad uuid length")
	}
	for i := 0; i < 16; i++ {
		n, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return out, err
		}
		out[i] = byte(n)
	}
	return out, nil
}

// --- gatekeeper ------------------------------------------------------------

// Gate is the connection-level gatekeeper. It enforces TCP/device limits,
// per-user and node-wide rate limits, and invokes the audit hook.
type Gate struct {
	mu           sync.Mutex
	limits       map[int64]User
	activeTCP    map[int64]int
	deviceRefs   map[int64]map[string]int
	limiters     map[int64]*rate.Limiter
	nodeLimiter  *rate.Limiter
	nodeSpeed    float64
	mode         string
	burst        int
	devicePrefix int
	audit        AuditChecker

	rejectsTCP    uint64
	rejectsDevice uint64
}

// GateMetrics is a snapshot of aggregate gatekeeper state.
type GateMetrics struct {
	Mode          string
	NodeSpeed     float64
	ActiveTCP     int
	Devices       int
	RejectsTCP    uint64
	RejectsDevice uint64
}

func NewGate(node NodeConfig, users []User) *Gate {
	g := &Gate{
		limits:       make(map[int64]User),
		activeTCP:    make(map[int64]int),
		deviceRefs:   make(map[int64]map[string]int),
		limiters:     make(map[int64]*rate.Limiter),
		nodeSpeed:    node.SpeedLimit,
		mode:         strings.ToLower(node.LimitsMode),
		burst:        node.BurstBytes,
		devicePrefix: node.DeviceIPv6Prefix,
		audit:        node.Audit,
	}
	if g.mode == "" {
		g.mode = "enforce"
	}
	g.nodeLimiter = makeLimiter(g.nodeSpeed, g.burst)
	if g.devicePrefix <= 0 || g.devicePrefix > 128 {
		g.devicePrefix = 128
	}
	g.UpdateLimitsMap(users)
	return g
}

func makeLimiter(mbps float64, burst int) *rate.Limiter {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := mbps * 1_000_000 / 8
	if burst <= 0 {
		burst = 65536
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec*0.2)+burst)
}

func (g *Gate) UpdateLimits(users map[int64]User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := make(map[int64]User, len(users))
	for _, u := range users {
		next[u.ID] = u
	}
	for id := range g.limiters {
		old, ok := g.limits[id]
		nu, still := next[id]
		if !ok || !still || old.SpeedLimit != nu.SpeedLimit {
			delete(g.limiters, id)
		}
	}
	g.limits = next
}

func (g *Gate) UpdateLimitsMap(users []User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := make(map[int64]User, len(users))
	for _, u := range users {
		next[u.ID] = u
	}
	for id := range g.limiters {
		old, ok := g.limits[id]
		nu, still := next[id]
		if !ok || !still || old.SpeedLimit != nu.SpeedLimit {
			delete(g.limiters, id)
		}
	}
	g.limits = next
}

func (g *Gate) Before(ctx context.Context, link *transport.Link) bool {
	in := session.InboundFromContext(ctx)
	if in == nil || in.User == nil {
		return true
	}
	id := userID(in.User.Email)
	if id == 0 {
		return true
	}
	network := targetNetwork(ctx)
	src := ""
	if in.Source.IsValid() && in.Source.Address != nil {
		src = in.Source.Address.String()
	}
	// Audit is independent of the limits mode and runs on the sniffed target.
	if g.audit != nil {
		dest := targetDestination(ctx)
		if dest.IsValid() && g.audit.Check(ctx, id, dest) {
			return false
		}
	}

	g.mu.Lock()
	if g.mode == "off" {
		g.mu.Unlock()
		return true
	}
	u := g.limits[id]
	if g.mode == "enforce" && network == "tcp" && u.TCPLimit > 0 && g.activeTCP[id] >= u.TCPLimit {
		g.rejectsTCP++
		g.mu.Unlock()
		return false
	}
	device := deviceKey(src, g.devicePrefix)
	if g.mode == "enforce" && u.DeviceLimit > 0 && device != "" {
		set := g.deviceRefs[id]
		if set == nil {
			set = make(map[string]int)
			g.deviceRefs[id] = set
		}
		if set[device] == 0 && len(set) >= u.DeviceLimit {
			g.rejectsDevice++
			g.mu.Unlock()
			return false
		}
		set[device]++
	}
	if network == "tcp" {
		g.activeTCP[id]++
	}
	var lims []*rate.Limiter
	if g.mode == "enforce" {
		if u.SpeedLimit > 0 {
			lim := g.limiters[id]
			if lim == nil {
				lim = makeLimiter(u.SpeedLimit, g.burst)
				if lim != nil {
					g.limiters[id] = lim
				}
			}
			if lim != nil {
				lims = append(lims, lim)
			}
		}
		if g.nodeLimiter != nil {
			lims = append(lims, g.nodeLimiter)
		}
	}
	g.mu.Unlock()

	if len(lims) > 0 {
		link.Reader = &rateReader{r: link.Reader, lims: lims, ctx: ctx}
		link.Writer = &rateWriter{w: link.Writer, lims: lims, ctx: ctx}
		if in != nil {
			in.CanSpliceCopy = 3
		}
	}
	return true
}

func (g *Gate) After(ctx context.Context, link *transport.Link) {
	if g.mode == "off" {
		return
	}
	in := session.InboundFromContext(ctx)
	if in == nil || in.User == nil {
		return
	}
	id := userID(in.User.Email)
	if id == 0 {
		return
	}
	network := targetNetwork(ctx)
	src := ""
	if in.Source.IsValid() && in.Source.Address != nil {
		src = in.Source.Address.String()
	}
	device := deviceKey(src, g.devicePrefix)
	g.mu.Lock()
	defer g.mu.Unlock()
	if network == "tcp" && g.activeTCP[id] > 0 {
		g.activeTCP[id]--
	}
	if device != "" {
		if set := g.deviceRefs[id]; set != nil {
			if set[device] > 0 {
				set[device]--
			}
			if set[device] == 0 {
				delete(set, device)
			}
			if len(set) == 0 {
				delete(g.deviceRefs, id)
			}
		}
	}
}

func (g *Gate) SetNodeSpeed(mbps float64) {
	g.mu.Lock()
	g.nodeSpeed = mbps
	g.nodeLimiter = makeLimiter(mbps, g.burst)
	// Existing user limiters are independent of the node cap.
	g.mu.Unlock()
}

func (g *Gate) Stats(id int64) (activeTCP int, devices int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeTCP[id], len(g.deviceRefs[id])
}

// Metrics returns aggregate counters for the admin endpoint.
func (g *Gate) Metrics() GateMetrics {
	g.mu.Lock()
	defer g.mu.Unlock()
	active, devices := 0, 0
	for _, n := range g.activeTCP {
		active += n
	}
	for _, set := range g.deviceRefs {
		devices += len(set)
	}
	return GateMetrics{Mode: g.mode, NodeSpeed: g.nodeSpeed, ActiveTCP: active, Devices: devices,
		RejectsTCP: g.rejectsTCP, RejectsDevice: g.rejectsDevice}
}

func deviceKey(src string, prefix int) string {
	if src == "" {
		return ""
	}
	addr, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(src), "[]"))
	if err != nil {
		return src
	}
	addr = addr.Unmap()
	if !addr.Is6() || prefix <= 0 || prefix >= 128 {
		return addr.String()
	}
	return netip.PrefixFrom(addr, prefix).Masked().String()
}

func targetDestination(ctx context.Context) xnet.Destination {
	obs := session.OutboundsFromContext(ctx)
	if len(obs) == 0 {
		return xnet.Destination{}
	}
	ob := obs[len(obs)-1]
	if ob == nil {
		return xnet.Destination{}
	}
	return ob.Target
}

func targetNetwork(ctx context.Context) string {
	obs := session.OutboundsFromContext(ctx)
	if len(obs) == 0 {
		return "tcp"
	}
	ob := obs[len(obs)-1]
	if ob != nil && ob.Target.IsValid() {
		n := strings.ToLower(ob.Target.Network.String())
		if n != "" {
			return n
		}
	}
	return "tcp"
}

func userID(email string) int64 {
	if !strings.HasPrefix(email, "u") {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimPrefix(email, "u"), 10, 64)
	return n
}

// wrapHandler overrides only Dispatch; all metadata delegates to the inner
// freedom handler. RemoveHandler does not close the inner handler, so this is
// safe and avoids needing a generated protobuf config for the M0 spike.
type wrapHandler struct {
	outbound.Handler
	gate *Gate
}

func (w *wrapHandler) Start() error { return nil }
func (w *wrapHandler) Close() error { return w.Handler.Close() }
func (w *wrapHandler) Dispatch(ctx context.Context, link *transport.Link) {
	if w.gate != nil && !w.gate.Before(ctx, link) {
		common.Interrupt(link.Writer)
		common.Interrupt(link.Reader)
		return
	}
	if w.gate != nil {
		defer w.gate.After(ctx, link)
	}
	w.Handler.Dispatch(ctx, link)
}

type rateReader struct {
	r    buf.Reader
	lims []*rate.Limiter
	ctx  context.Context
}

func (r *rateReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.r.ReadMultiBuffer()
	if err != nil || mb.IsEmpty() {
		return mb, err
	}
	for _, lim := range r.lims {
		if err := lim.WaitN(r.ctx, int(mb.Len())); err != nil {
			return mb, err
		}
	}
	return mb, nil
}

type rateWriter struct {
	w    buf.Writer
	lims []*rate.Limiter
	ctx  context.Context
}

func (w *rateWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if !mb.IsEmpty() {
		for _, lim := range w.lims {
			if err := lim.WaitN(w.ctx, int(mb.Len())); err != nil {
				return err
			}
		}
	}
	return w.w.WriteMultiBuffer(mb)
}
