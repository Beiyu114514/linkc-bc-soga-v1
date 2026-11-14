// Package mockpanel implements a minimal, source-compatible emulator of the
// target site's soga-v1 WebAPI (SSPanel-UIM + SogaController).
//
// It is a test fixture: it reproduces the behaviours that matter for the node
// agent (ETag/304, empty users, over-quota filtering, status.load, raw audit
// rules, traffic online count, alive-IP upsert, daily audit dedupe), without a
// database or any of the site's UI.
package mockpanel

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the node definition returned by GET /api/v1/node.
type Config struct {
	APIKey       string
	NodeID       int64
	NodeType     string
	PullInterval int
	PushInterval int
	SpeedLimit   float64
	TrafficRate  float64

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

	NodeSpeedLimit     float64
	NodeConnector      int
	NodeClass          int
	NodeGroup          int
	NodeBandwidth      int64
	NodeBandwidthLimit int64
	NodeIP             string // comma-separated
	NodeHeartbeat      int64

	WebAPIDisabled bool
	CheckNodeIP    bool
	KeepConnect    bool
}

// User mirrors the subset of site user fields that affect the node.
type User struct {
	ID             int64
	UUID           string
	SpeedLimit     float64 // node_speedlimit, Mbps
	DeviceLimit    int     // node_connector
	TCPLimit       int     // site currently hardcodes 0
	TransferEnable int64
	U              int64
	D              int64
	Enable         bool
	ExpireIn       time.Time
	IsAdmin        bool
	Class          int
	NodeGroup      int
	IsMultiUser    int

	// Extra fields returned by the site; the agent must ignore them for vless.
	Method        string
	Protocol      string
	ProtocolParam string
	Obfs          string
	ObfsParam     string
}

// Rule mirrors DetectRule / detect_list.
type Rule struct {
	ID    int64
	Name  string
	Text  string
	Regex string
	Type  int
}

// AliveIP mirrors the site's alive_ip row.
type AliveIP struct {
	UserID   int64
	NodeID   int64
	IP       string
	DateTime int64
}

// DetectLog mirrors detect_log.
type DetectLog struct {
	UserID   int64
	ListID   int64
	NodeID   int64
	DateTime int64
}

// TrafficLog mirrors user_traffic_log.
type TrafficLog struct {
	UserID  int64
	U       int64
	D       int64
	NodeID  int64
	Rate    float64
	LogTime int64
}

// NodeOnlineLog mirrors ss_node_online_log.
type NodeOnlineLog struct {
	NodeID     int64
	OnlineUser int
	LogTime    int64
}

// NodeInfoLog mirrors ss_node_info.
type NodeInfoLog struct {
	NodeID  int64
	Load    string
	Uptime  float64
	LogTime int64
}

// Panel holds all mutable state. It is safe for concurrent use.
type Panel struct {
	mu sync.Mutex

	cfg   Config
	users map[int64]*User
	rules []Rule
	white []string

	alive    []AliveIP
	detects  []DetectLog
	traffic  []TrafficLog
	online   []NodeOnlineLog
	infoLogs []NodeInfoLog

	// requests counts endpoint calls; useful in tests.
	requests map[string]int

	// faults are injected failures consumed by ServeHTTP.
	faults []fault
}

func New(cfg Config) *Panel {
	if cfg.NodeType == "" {
		cfg.NodeType = "vless"
	}
	if cfg.PullInterval == 0 {
		cfg.PullInterval = 60
	}
	if cfg.PushInterval == 0 {
		cfg.PushInterval = 60
	}
	if cfg.TrafficRate == 0 {
		cfg.TrafficRate = 1
	}
	if cfg.StreamType == "" {
		cfg.StreamType = "tcp"
	}
	if cfg.TLSType == "" {
		cfg.TLSType = "none"
	}
	return &Panel{
		cfg:      cfg,
		users:    make(map[int64]*User),
		requests: make(map[string]int),
	}
}

// ServeHTTP implements http.Handler.
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests[r.Method+" "+r.URL.Path]++
	if status, ok := p.takeFault(r.URL.Path); ok {
		p.mu.Unlock()
		p.error(w, status, "injected fault")
		return
	}
	p.mu.Unlock()

	switch r.URL.Path {
	case "/api/v1/node":
		p.getNode(w, r)
	case "/api/v1/users":
		p.getUsers(w, r)
	case "/api/v1/audit_rules":
		p.getAuditRules(w, r)
	case "/api/v1/white_list":
		p.getWhiteList(w, r)
	case "/api/v1/traffic":
		p.postTraffic(w, r)
	case "/api/v1/alive_ip":
		p.postAliveIP(w, r)
	case "/api/v1/audit_log":
		p.postAuditLog(w, r)
	case "/api/v1/status":
		p.postStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

// --- test controls ---------------------------------------------------------

func (p *Panel) SetUsers(users ...User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = make(map[int64]*User, len(users))
	for i := range users {
		u := users[i]
		if u.ExpireIn.IsZero() {
			u.ExpireIn = time.Now().Add(24 * time.Hour)
		}
		p.users[u.ID] = &u
	}
}

func (p *Panel) AddUser(u User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u.ExpireIn.IsZero() {
		u.ExpireIn = time.Now().Add(24 * time.Hour)
	}
	p.users[u.ID] = &u
}

func (p *Panel) SetNodeConfig(cfg Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Preserve only the identity/auth part if omitted.
	if cfg.NodeID == 0 {
		cfg.NodeID = p.cfg.NodeID
	}
	if cfg.APIKey == "" {
		cfg.APIKey = p.cfg.APIKey
	}
	if cfg.NodeType == "" {
		cfg.NodeType = p.cfg.NodeType
	}
	if cfg.PullInterval == 0 {
		cfg.PullInterval = p.cfg.PullInterval
	}
	if cfg.PushInterval == 0 {
		cfg.PushInterval = p.cfg.PushInterval
	}
	if cfg.TrafficRate == 0 {
		cfg.TrafficRate = p.cfg.TrafficRate
	}
	p.cfg = cfg
}

func (p *Panel) SetRules(rules ...Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append([]Rule(nil), rules...)
}

func (p *Panel) SetWhiteList(items ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.white = append([]string(nil), items...)
}

func (p *Panel) SetKeepConnect(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.KeepConnect = v
}

func (p *Panel) SetNodeBandwidth(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.NodeBandwidth = v
}

func (p *Panel) SetNodeBandwidthLimit(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.NodeBandwidthLimit = v
}

func (p *Panel) SetCheckNodeIP(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.CheckNodeIP = v
}

func (p *Panel) SetWebAPIDisabled(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.WebAPIDisabled = v
}

// SetFault makes requests to path return status instead of the real handler.
// An empty path matches every request. times <= 0 lasts until ClearFaults.
// It simulates panel-side failures (500/502/503/429) for chaos tests.
func (p *Panel) SetFault(path string, status, times int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = append(p.faults, fault{path: path, status: status, times: times})
}

// ClearFaults removes all injected faults.
func (p *Panel) ClearFaults() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = nil
}

// fault is one pending injected failure.
type fault struct {
	path   string
	status int
	times  int
}

// takeFault consumes one fault matching path; it returns the status to send
// and whether a fault was taken.
func (p *Panel) takeFault(path string) (int, bool) {
	for i := range p.faults {
		f := &p.faults[i]
		if f.path != "" && f.path != path {
			continue
		}
		status := f.status
		if f.times > 0 {
			f.times--
			if f.times == 0 {
				p.faults = append(p.faults[:i], p.faults[i+1:]...)
			}
		}
		return status, true
	}
	return 0, false
}

// Snapshot returns a deep-enough copy for assertions.
func (p *Panel) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Snapshot{
		Config:    p.cfg,
		Alive:     append([]AliveIP(nil), p.alive...),
		Detects:   append([]DetectLog(nil), p.detects...),
		Traffic:   append([]TrafficLog(nil), p.traffic...),
		Online:    append([]NodeOnlineLog(nil), p.online...),
		InfoLogs:  append([]NodeInfoLog(nil), p.infoLogs...),
		Users:     make(map[int64]User, len(p.users)),
		Rules:     append([]Rule(nil), p.rules...),
		WhiteList: append([]string(nil), p.white...),
		Requests:  make(map[string]int, len(p.requests)),
	}
	for k, v := range p.users {
		s.Users[k] = *v
	}
	for k, v := range p.requests {
		s.Requests[k] = v
	}
	return s
}

type Snapshot struct {
	Config    Config
	Users     map[int64]User
	Rules     []Rule
	WhiteList []string
	Alive     []AliveIP
	Detects   []DetectLog
	Traffic   []TrafficLog
	Online    []NodeOnlineLog
	InfoLogs  []NodeInfoLog
	Requests  map[string]int
}

// --- endpoints -------------------------------------------------------------

func (p *Panel) getNode(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.error(w, 404, "node not found")
		return
	}
	node.NodeHeartbeat = time.Now().Unix()
	cfg := *node
	p.mu.Unlock()

	payload := map[string]any{
		"basic": map[string]any{
			"pull_interval": cfg.PullInterval,
			"push_interval": cfg.PushInterval,
			"speed_limit":   cfg.SpeedLimit,
		},
		"config": map[string]any{
			"port":         cfg.Port,
			"stream_type":  cfg.StreamType,
			"tls_type":     cfg.TLSType,
			"flow":         cfg.Flow,
			"server_names": cfg.ServerNames,
			"private_key":  cfg.PrivateKey,
			"short_ids":    cfg.ShortIDs,
			"dest":         cfg.Dest,
		},
		"node_type":    cfg.NodeType,
		"traffic_rate": cfg.TrafficRate,
	}
	body := payload["config"].(map[string]any)
	if cfg.Path != "" {
		body["path"] = cfg.Path
	}
	if cfg.ServiceName != "" {
		body["service_name"] = cfg.ServiceName
	}
	if cfg.XVER != 0 {
		body["xver"] = cfg.XVER
	}
	p.writeJSONWithETag(w, r, payload)
}

func (p *Panel) getUsers(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.error(w, 404, "node not found")
		return
	}
	node.NodeHeartbeat = time.Now().Unix()
	if node.NodeBandwidthLimit != 0 && node.NodeBandwidthLimit < node.NodeBandwidth {
		p.mu.Unlock()
		p.writeJSONWithETag(w, r, []any{})
		return
	}

	now := time.Now()
	nodeType := p.nodeTypeLocked(r, node)
	out := make([]map[string]any, 0, len(p.users))
	for _, u := range p.users {
		if !u.Enable || u.ExpireIn.Before(now) {
			continue
		}
		if u.Class < cfgClass(node) {
			continue
		}
		if node.NodeGroup != 0 && u.NodeGroup != node.NodeGroup && !u.IsAdmin {
			continue
		}
		if u.TransferEnable > 0 && u.TransferEnable <= u.U+u.D {
			if !p.cfg.KeepConnect {
				continue
			}
			u = copyUser(u)
			u.SpeedLimit = 1
		}
		item := map[string]any{
			"id":             u.ID,
			"speed_limit":    u.SpeedLimit,
			"device_limit":   u.DeviceLimit,
			"tcp_limit":      0,
			"method":         u.Method,
			"protocol":       u.Protocol,
			"protocol_param": u.ProtocolParam,
			"obfs":           u.Obfs,
			"obfs_param":     u.ObfsParam,
		}
		if nodeType == "vless" || nodeType == "vmess" {
			item["uuid"] = u.UUID
		} else {
			item["password"] = u.UUID
		}
		out = append(out, item)
	}
	p.mu.Unlock()
	p.writeJSONWithETag(w, r, out)
}

func (p *Panel) getAuditRules(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	p.mu.Lock()
	rules := append([]Rule(nil), p.rules...)
	p.mu.Unlock()
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		text := rule.Regex
		if text == "" {
			text = rule.Text
		}
		if text == "" {
			continue
		}
		out = append(out, map[string]any{"id": rule.ID, "rule": text})
	}
	p.writeJSONWithETag(w, r, out)
}

func (p *Panel) getWhiteList(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	p.mu.Lock()
	white := append([]string(nil), p.white...)
	p.mu.Unlock()
	p.writeJSONWithETag(w, r, white)
}

func (p *Panel) postTraffic(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	logs, err := readList(r)
	if err != nil {
		p.result(w, 1, "update failed")
		return
	}
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.result(w, 404, "node not found")
		return
	}
	now := time.Now().Unix()
	rate := node.TrafficRate
	total := int64(0)
	online := 0
	for _, raw := range logs {
		id := asInt64(raw["id"])
		if id == 0 {
			id = asInt64(raw["user_id"])
		}
		if id <= 0 {
			continue
		}
		u := p.users[id]
		if u == nil {
			continue
		}
		up := asInt64(raw["u"])
		down := asInt64(raw["d"])
		if up < 0 {
			up = 0
		}
		if down < 0 {
			down = 0
		}
		u.U += int64(float64(up) * rate)
		u.D += int64(float64(down) * rate)
		total += up + down
		online++
		p.traffic = append(p.traffic, TrafficLog{UserID: id, U: up, D: down, NodeID: node.NodeID, Rate: rate, LogTime: now})
	}
	node.NodeBandwidth += total
	node.NodeHeartbeat = now
	p.online = append(p.online, NodeOnlineLog{NodeID: node.NodeID, OnlineUser: online, LogTime: now})
	p.mu.Unlock()
	p.result(w, 0, "")
}

func (p *Panel) postAliveIP(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	items, err := readList(r)
	if err != nil {
		p.result(w, 1, "update failed")
		return
	}
	now := time.Now().Unix()
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.result(w, 404, "node not found")
		return
	}
	for _, raw := range items {
		id := asInt64(raw["id"])
		if id == 0 {
			id = asInt64(raw["user_id"])
		}
		if id <= 0 {
			continue
		}
		var ips []string
		if v, ok := raw["ips"]; ok {
			switch vv := v.(type) {
			case []any:
				for _, x := range vv {
					ips = append(ips, fmt.Sprint(x))
				}
			case []string:
				ips = append(ips, vv...)
			}
		} else if v, ok := raw["ip"]; ok {
			ips = append(ips, fmt.Sprint(v))
		}
		seen := map[string]bool{}
		for _, ip := range ips {
			ip = strings.TrimPrefix(ip, "::ffff:")
			if net.ParseIP(ip) == nil || seen[ip] {
				continue
			}
			seen[ip] = true
			found := false
			for i := range p.alive {
				a := &p.alive[i]
				if a.UserID == id && a.NodeID == node.NodeID && a.IP == ip && a.DateTime >= now-60 {
					a.DateTime = now
					found = true
					break
				}
			}
			if !found {
				p.alive = append(p.alive, AliveIP{UserID: id, NodeID: node.NodeID, IP: ip, DateTime: now})
			}
		}
	}
	node.NodeHeartbeat = now
	p.mu.Unlock()
	p.result(w, 0, "")
}

func (p *Panel) postAuditLog(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	items, err := readList(r)
	if err != nil {
		p.result(w, 1, "update failed")
		return
	}
	now := time.Now().Unix()
	dayStart := now - (now % 86400)
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.result(w, 404, "node not found")
		return
	}
	for _, raw := range items {
		userID := asInt64(raw["user_id"])
		if userID == 0 {
			userID = asInt64(raw["id"])
		}
		ruleID := asInt64(raw["audit_id"])
		if ruleID == 0 {
			ruleID = asInt64(raw["list_id"])
		}
		if ruleID == 0 {
			ruleID = asInt64(raw["rule_id"])
		}
		if userID <= 0 || ruleID <= 0 {
			continue
		}
		exists := false
		for _, d := range p.detects {
			if d.UserID == userID && d.ListID == ruleID && d.NodeID == node.NodeID && d.DateTime >= dayStart {
				exists = true
				break
			}
		}
		if !exists {
			p.detects = append(p.detects, DetectLog{UserID: userID, ListID: ruleID, NodeID: node.NodeID, DateTime: now})
		}
	}
	node.NodeHeartbeat = now
	p.mu.Unlock()
	p.result(w, 0, "")
}

func (p *Panel) postStatus(w http.ResponseWriter, r *http.Request) {
	if !p.guard(w, r) {
		return
	}
	payload, err := readMap(r)
	if err != nil {
		p.result(w, 1, "update failed")
		return
	}
	load := payload["load"]
	if load == nil {
		load = payload["cpu"]
	}
	loadStr := ""
	switch v := load.(type) {
	case map[string]any:
		cpu := v["cpu"]
		if cpu == nil {
			cpu = v["load"]
		}
		mem := v["mem"]
		if mem == nil {
			mem = v["memory"]
		}
		loadStr = fmt.Sprint(cpu)
		if mem != nil {
			loadStr += "/" + fmt.Sprint(mem)
		}
	default:
		loadStr = fmt.Sprint(load)
	}
	if len(loadStr) > 32 {
		loadStr = loadStr[:32]
	}
	uptime := asFloat64(payload["uptime"])
	p.mu.Lock()
	node, ok := p.nodeForRequest(r)
	if !ok {
		p.mu.Unlock()
		p.result(w, 404, "node not found")
		return
	}
	node.NodeHeartbeat = time.Now().Unix()
	p.infoLogs = append(p.infoLogs, NodeInfoLog{NodeID: node.NodeID, Load: loadStr, Uptime: uptime, LogTime: time.Now().Unix()})
	p.mu.Unlock()
	p.result(w, 0, "")
}

// --- helpers ---------------------------------------------------------------

func (p *Panel) guard(w http.ResponseWriter, r *http.Request) bool {
	key := r.Header.Get("API-KEY")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	if key == "" {
		p.error(w, 401, "api key is required")
		return false
	}
	if key != p.cfg.APIKey {
		p.error(w, 401, "api key is invalid")
		return false
	}
	if p.cfg.WebAPIDisabled {
		p.error(w, 503, "webapi is disabled")
		return false
	}
	if p.cfg.CheckNodeIP {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != "127.0.0.1" && host != "::1" && !containsIP(p.cfg.NodeIP, host) {
			p.error(w, 403, "node ip is invalid")
			return false
		}
	}
	return true
}

func (p *Panel) nodeForRequest(r *http.Request) (*Config, bool) {
	id := asInt64(r.Header.Get("NODE-ID"))
	if id == 0 {
		id = asInt64(r.URL.Query().Get("node_id"))
	}
	if id != 0 {
		if id != p.cfg.NodeID {
			return nil, false
		}
		return &p.cfg, true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if containsIP(p.cfg.NodeIP, host) {
		return &p.cfg, true
	}
	return nil, false
}

func (p *Panel) nodeTypeLocked(r *http.Request, node *Config) string {
	t := strings.ToLower(strings.TrimSpace(r.Header.Get("NODE-TYPE")))
	if t == "" {
		return node.NodeType
	}
	switch t {
	case "ss":
		return "shadowsocks"
	case "ssr":
		return "shadowsocksr"
	case "vless", "vmess", "trojan", "shadowsocks", "shadowsocksr":
		return t
	}
	return node.NodeType
}

func (p *Panel) writeJSONWithETag(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		p.error(w, 500, "encode failed")
		return
	}
	sum := sha1.Sum(b)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	if r.Header.Get("IF-NONE-MATCH") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (p *Panel) result(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message})
}

func (p *Panel) error(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "message": message})
}

func readMap(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case []any:
		if len(x) > 0 {
			if m, ok := x[0].(map[string]any); ok {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("expected object or non-empty array")
}

func readList(r *http.Request) ([]map[string]any, error) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out, nil
	case map[string]any:
		if data, ok := x["data"]; ok {
			if arr, ok := data.([]any); ok {
				out := make([]map[string]any, 0, len(arr))
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						out = append(out, m)
					}
				}
				return out, nil
			}
		}
		return []map[string]any{x}, nil
	default:
		return nil, fmt.Errorf("expected array or object")
	}
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

func asFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

func containsIP(list, ip string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == ip {
			return true
		}
	}
	return false
}

func cfgClass(c *Config) int { return c.NodeClass }

func copyUser(u *User) *User {
	v := *u
	return &v
}
