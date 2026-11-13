// Package engine is an M0 spike around an embedded Xray-core instance.
//
// It validates the highest-risk integration points:
//   - building a VLESS inbound from panel data and starting Xray in-process;
//   - injecting a Gatekeeper wrapper around the freedom outbound;
//   - hot user add/remove through proxy.UserManager;
//   - reading/resetting per-user traffic counters and online IP maps;
//   - adding/removing inbounds on the same port via SO_REUSEPORT.
//
// It is intentionally a spike, not the production agent.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
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

// NodeConfig is the subset of GET /api/v1/node the spike needs.
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

	SpeedLimit   float64 // node-wide Mbps
	AllowPrivate bool    // M0: allow loopback/private targets in tests
	// TLS material is local; the site does not send certificate paths.
	CertFile string
	KeyFile  string
	// Inline PEM is useful for tests; it avoids Xray's file hot-reload ticker.
	CertPEM string
	KeyPEM  string
	// Client-side pinned cert hash (for TLS e2e), optional.
	PinnedPeerCertSha256 string
	ServerName           string
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
		if err := um.AddUser(ctx, memoryUser(nu, e.flow)); err != nil {
			return fmt.Errorf("add user %d: %w", id, err)
		}
	}
	e.users = next
	e.gate.UpdateLimits(next)
	return nil
}

// AddInbound adds a new VLESS inbound (same or different port), then returns
// so the caller can remove the old tag. Same-port overlap relies on Xray's
// SO_REUSEPORT listener.
func (e *Engine) AddInbound(ctx context.Context, node NodeConfig, users []User, tag string) error {
	if node.Listen == "" {
		node.Listen = "127.0.0.1"
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
	e.mu.Lock()
	e.tag = tag
	e.flow = node.Flow
	e.port = node.Port
	e.users = make(map[int64]User, len(users))
	for _, u := range users {
		e.users[u.ID] = u
	}
	e.gate.UpdateLimitsMap(users)
	e.mu.Unlock()
	return nil
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

// Gate exposes the M0 gatekeeper for assertions.
func (e *Engine) Gate() *Gate { return e.gate }

// SetNodeSpeed updates the node-wide speed cap used for new limiters.
func (e *Engine) SetNodeSpeed(mbps float64) {
	e.gate.mu.Lock()
	e.gate.nodeSpeed = mbps
	e.gate.mu.Unlock()
}

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

	settings := map[string]any{
		"decryption": "none",
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
			"enabled":      true,
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
		stream["grpcSettings"] = map[string]any{"serviceName": node.ServiceName}
	case "xhttp":
		stream["xhttpSettings"] = map[string]any{"path": node.Path, "mode": "auto"}
	}

	switch strings.ToLower(node.TLSType) {
	case "", "none":
	case "tls":
		stream["security"] = "tls"
		tls := map[string]any{}
		switch {
		case node.CertPEM != "" && node.KeyPEM != "":
			tls["certificates"] = []any{map[string]any{
				"certificate": []string{node.CertPEM},
				"key":         []string{node.KeyPEM},
			}}
		case node.CertFile != "" && node.KeyFile != "":
			tls["certificates"] = []any{map[string]any{
				"certificateFile": node.CertFile,
				"keyFile":         node.KeyFile,
			}}
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

func memoryUser(u User, flow string) *protocol.MemoryUser {
	return &protocol.MemoryUser{
		Email: email(u.ID),
		Level: 0,
		Account: &vless.MemoryAccount{
			ID:   protocol.NewID(mustUUID(u.UUID)),
			Flow: flow,
		},
	}
}

func mustUUID(s string) [16]byte {
	id, err := parseUUID(s)
	if err != nil {
		panic(err)
	}
	return id
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

// Gate is a minimal connection-level gatekeeper. It enforces TCP/device
// limits and can rate-limit user links. It is intentionally small; the
// production implementation will be split out and hardened.
type Gate struct {
	mu         sync.Mutex
	limits     map[int64]User
	activeTCP  map[int64]int
	deviceRefs map[int64]map[string]int
	limiters   map[int64]*rate.Limiter
	nodeSpeed  float64
}

func NewGate(node NodeConfig, users []User) *Gate {
	g := &Gate{
		limits:     make(map[int64]User),
		activeTCP:  make(map[int64]int),
		deviceRefs: make(map[int64]map[string]int),
		limiters:   make(map[int64]*rate.Limiter),
		nodeSpeed:  node.SpeedLimit,
	}
	g.UpdateLimitsMap(users)
	return g
}

func (g *Gate) UpdateLimits(users map[int64]User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = make(map[int64]User, len(users))
	for _, u := range users {
		g.limits[u.ID] = u
	}
	for id := range g.limiters {
		if _, ok := g.limits[id]; !ok {
			delete(g.limiters, id)
		}
	}
}

func (g *Gate) UpdateLimitsMap(users []User) {
	for _, u := range users {
		g.limits[u.ID] = u
	}
	for id := range g.limits {
		found := false
		for _, u := range users {
			if u.ID == id {
				found = true
				break
			}
		}
		if !found {
			delete(g.limits, id)
			delete(g.limiters, id)
		}
	}
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
	g.mu.Lock()
	u := g.limits[id]
	if network == "tcp" && u.TCPLimit > 0 && g.activeTCP[id] >= u.TCPLimit {
		g.mu.Unlock()
		return false
	}
	if u.DeviceLimit > 0 && src != "" {
		set := g.deviceRefs[id]
		if set == nil {
			set = make(map[string]int)
			g.deviceRefs[id] = set
		}
		if set[src] == 0 && len(set) >= u.DeviceLimit {
			g.mu.Unlock()
			return false
		}
		set[src]++
	}
	if network == "tcp" {
		g.activeTCP[id]++
	}
	// Add a rate limiter if needed.
	var lim *rate.Limiter
	if u.SpeedLimit > 0 || g.nodeSpeed > 0 {
		lim = g.limiters[id]
		if lim == nil {
			// User limits are independent per direction in this spike; use
			// the stricter of user/node for simplicity.
			mb := u.SpeedLimit
			if g.nodeSpeed > 0 && (mb == 0 || g.nodeSpeed < mb) {
				mb = g.nodeSpeed
			}
			if mb > 0 {
				bytesPerSec := mb * 1_000_000 / 8
				lim = rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec*0.2)+65536)
				g.limiters[id] = lim
			}
		}
	}
	g.mu.Unlock()

	if lim != nil {
		link.Reader = &rateReader{r: link.Reader, lim: lim, ctx: ctx}
		link.Writer = &rateWriter{w: link.Writer, lim: lim, ctx: ctx}
		if in != nil {
			in.CanSpliceCopy = 3
		}
	}
	return true
}

func (g *Gate) After(ctx context.Context, link *transport.Link) {
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
	g.mu.Lock()
	defer g.mu.Unlock()
	if network == "tcp" && g.activeTCP[id] > 0 {
		g.activeTCP[id]--
	}
	if src != "" {
		if set := g.deviceRefs[id]; set != nil {
			if set[src] > 0 {
				set[src]--
			}
			if set[src] == 0 {
				delete(set, src)
			}
			if len(set) == 0 {
				delete(g.deviceRefs, id)
			}
		}
	}
}

func (g *Gate) Stats(id int64) (activeTCP int, devices int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeTCP[id], len(g.deviceRefs[id])
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
	r   buf.Reader
	lim *rate.Limiter
	ctx context.Context
}

func (r *rateReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.r.ReadMultiBuffer()
	if err != nil || mb.IsEmpty() {
		return mb, err
	}
	if err := r.lim.WaitN(r.ctx, int(mb.Len())); err != nil {
		return mb, err
	}
	return mb, nil
}

type rateWriter struct {
	w   buf.Writer
	lim *rate.Limiter
	ctx context.Context
}

func (w *rateWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if !mb.IsEmpty() {
		if err := w.lim.WaitN(w.ctx, int(mb.Len())); err != nil {
			return err
		}
	}
	return w.w.WriteMultiBuffer(mb)
}
