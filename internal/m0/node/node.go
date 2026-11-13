// Package node is a minimal M0 vertical slice: it pulls node/users from a
// soga-v1 panel, runs the embedded Xray engine, and pushes traffic/online/status.
// It is a spike, not the production agent.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
)

type Config struct {
	PanelURL string
	APIKey   string
	NodeID   string
	NodeType string
	Listen   string
	HTTP     *http.Client
}

type Node struct {
	cfg Config

	mu      sync.Mutex
	client  *http.Client
	etags   map[string]string
	cached  map[string][]byte
	engine  *engine.Engine
	nodeCfg engine.NodeConfig
	users   []engine.User
	lastCPU cpuSample
}

type nodeResponse struct {
	Basic struct {
		PullInterval int     `json:"pull_interval"`
		PushInterval int     `json:"push_interval"`
		SpeedLimit   float64 `json:"speed_limit"`
	} `json:"basic"`
	Config struct {
		Port        int      `json:"port"`
		StreamType  string   `json:"stream_type"`
		TLSType     string   `json:"tls_type"`
		Flow        string   `json:"flow"`
		ServerNames []string `json:"server_names"`
		PrivateKey  string   `json:"private_key"`
		ShortIDs    []string `json:"short_ids"`
		Dest        string   `json:"dest"`
		Path        string   `json:"path"`
		ServiceName string   `json:"service_name"`
		XVER        int      `json:"xver"`
	} `json:"config"`
	NodeType    string  `json:"node_type"`
	TrafficRate float64 `json:"traffic_rate"`
}

type userResponse struct {
	ID          int64   `json:"id"`
	UUID        string  `json:"uuid"`
	SpeedLimit  float64 `json:"speed_limit"`
	DeviceLimit int     `json:"device_limit"`
	TCPLimit    int     `json:"tcp_limit"`
}

// Status is the site-compatible status payload. Load is CPU fraction.
type Status struct {
	CPUPercent float64 `json:"cpu"`
	MemTotal   int64   `json:"-"`
	MemUsed    int64   `json:"-"`
	SwapTotal  int64   `json:"-"`
	SwapUsed   int64   `json:"-"`
	DiskTotal  int64   `json:"-"`
	DiskUsed   int64   `json:"-"`
	Uptime     int64   `json:"uptime"`
}

func New(cfg Config) *Node {
	if cfg.NodeType == "" {
		cfg.NodeType = "vless"
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1"
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	n := &Node{
		cfg:    cfg,
		client: hc,
		etags:  make(map[string]string),
		cached: make(map[string][]byte),
	}
	n.lastCPU = readCPUSample()
	return n
}

// Sync fetches /node and /users, then starts or updates the engine.
func (n *Node) Sync(ctx context.Context) error {
	var nr nodeResponse
	if err := n.getJSON(ctx, "/api/v1/node", &nr); err != nil {
		return fmt.Errorf("fetch node: %w", err)
	}
	var ur []userResponse
	if err := n.getJSON(ctx, "/api/v1/users", &ur); err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}
	if nr.NodeType != "" && nr.NodeType != "vless" {
		return fmt.Errorf("panel node_type %q != vless", nr.NodeType)
	}
	nodeCfg := engine.NodeConfig{
		Listen:       n.cfg.Listen,
		Port:         nr.Config.Port,
		StreamType:   nr.Config.StreamType,
		TLSType:      nr.Config.TLSType,
		Flow:         nr.Config.Flow,
		ServerNames:  nr.Config.ServerNames,
		PrivateKey:   nr.Config.PrivateKey,
		ShortIDs:     nr.Config.ShortIDs,
		Dest:         nr.Config.Dest,
		Path:         nr.Config.Path,
		ServiceName:  nr.Config.ServiceName,
		XVER:         nr.Config.XVER,
		SpeedLimit:   nr.Basic.SpeedLimit,
		AllowPrivate: true, // M0 e2e uses local/private targets
	}
	users := make([]engine.User, 0, len(ur))
	for _, u := range ur {
		users = append(users, engine.User{
			ID:          u.ID,
			UUID:        u.UUID,
			SpeedLimit:  u.SpeedLimit,
			DeviceLimit: u.DeviceLimit,
			TCPLimit:    u.TCPLimit,
		})
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.engine == nil {
		e, err := engine.Start(ctx, nodeCfg, users)
		if err != nil {
			return err
		}
		n.engine = e
		n.nodeCfg = nodeCfg
		n.users = users
		return nil
	}
	if inboundChanged(n.nodeCfg, nodeCfg) {
		newTag := "vless-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if err := n.engine.AddInbound(ctx, nodeCfg, users, newTag); err != nil {
			return fmt.Errorf("add inbound: %w", err)
		}
		old := n.engine.Tag()
		if err := n.engine.RemoveInbound(ctx, old); err != nil {
			return fmt.Errorf("remove old inbound: %w", err)
		}
		n.nodeCfg = nodeCfg
		n.users = users
		n.engine.SetNodeSpeed(nodeCfg.SpeedLimit)
		return nil
	}
	if err := n.engine.ApplyUsers(ctx, users); err != nil {
		return err
	}
	n.nodeCfg = nodeCfg
	n.users = users
	n.engine.SetNodeSpeed(nodeCfg.SpeedLimit)
	return nil
}

func inboundChanged(a, b engine.NodeConfig) bool {
	return a.Port != b.Port ||
		a.StreamType != b.StreamType ||
		a.TLSType != b.TLSType ||
		a.Flow != b.Flow ||
		a.PrivateKey != b.PrivateKey ||
		a.Dest != b.Dest ||
		a.Path != b.Path ||
		a.ServiceName != b.ServiceName ||
		strings.Join(a.ServerNames, ",") != strings.Join(b.ServerNames, ",") ||
		strings.Join(a.ShortIDs, ",") != strings.Join(b.ShortIDs, ",")
}

// PushTraffic sends deltas. The caller must only call this with deltas read
// from ReadTraffic (the spike does not persist a journal).
func (n *Node) PushTraffic(ctx context.Context, deltas map[int64]engine.Traffic) error {
	if len(deltas) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(deltas))
	for id, t := range deltas {
		items = append(items, map[string]any{"id": id, "u": t.Up, "d": t.Down})
	}
	return n.post(ctx, "/api/v1/traffic", items)
}

// PushAliveIP sends the current online IP snapshot.
func (n *Node) PushAliveIP(ctx context.Context, ips map[int64][]string) error {
	if len(ips) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(ips))
	for id, list := range ips {
		items = append(items, map[string]any{"id": id, "ips": list})
	}
	return n.post(ctx, "/api/v1/alive_ip", items)
}

// PushStatus sends a site-compatible status payload.
func (n *Node) PushStatus(ctx context.Context, st Status) error {
	load := st.CPUPercent / 100
	payload := map[string]any{
		"load":   fmt.Sprintf("%.6f", load),
		"cpu":    st.CPUPercent,
		"mem":    map[string]any{"total": st.MemTotal, "used": st.MemUsed},
		"swap":   map[string]any{"total": st.SwapTotal, "used": st.SwapUsed},
		"disk":   map[string]any{"total": st.DiskTotal, "used": st.DiskUsed},
		"uptime": st.Uptime,
	}
	return n.post(ctx, "/api/v1/status", payload)
}

// CollectStatus samples host status. The first call after New returns 0 CPU.
func (n *Node) CollectStatus() Status {
	n.mu.Lock()
	prev := n.lastCPU
	cur := readCPUSample()
	n.lastCPU = cur
	n.mu.Unlock()

	pct := 0.0
	if prev.total > 0 && cur.total > prev.total {
		dt := float64(cur.total - prev.total)
		db := float64(cur.busy - prev.busy)
		pct = db / dt * 100
	}
	memTotal, memAvail, swapTotal, swapFree := readMemInfo()
	diskTotal, diskUsed := readDisk("/")
	uptime := readUptime()
	return Status{
		CPUPercent: pct,
		MemTotal:   memTotal,
		MemUsed:    memTotal - memAvail,
		SwapTotal:  swapTotal,
		SwapUsed:   swapTotal - swapFree,
		DiskTotal:  diskTotal,
		DiskUsed:   diskUsed,
		Uptime:     uptime,
	}
}

// Engine returns the embedded engine (nil before the first Sync).
func (n *Node) Engine() *engine.Engine {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.engine
}

// NodeConfig returns the last applied node config.
func (n *Node) NodeConfig() engine.NodeConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.nodeCfg
}

// Users returns the last applied user list.
func (n *Node) Users() []engine.User {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]engine.User(nil), n.users...)
}

// Close stops the engine.
func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.engine == nil {
		return nil
	}
	err := n.engine.Close()
	n.engine = nil
	return err
}

// --- HTTP ------------------------------------------------------------------

func (n *Node) getJSON(ctx context.Context, path string, out any) error {
	body, err := n.get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (n *Node) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(n.cfg.PanelURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	n.setHeaders(req)
	n.mu.Lock()
	if et := n.etags[path]; et != "" {
		req.Header.Set("IF-NONE-MATCH", et)
	}
	n.mu.Unlock()

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		n.mu.Lock()
		body := append([]byte(nil), n.cached[path]...)
		n.mu.Unlock()
		if len(body) == 0 {
			return nil, fmt.Errorf("304 without cache for %s", path)
		}
		return body, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.cached[path] = append([]byte(nil), body...)
	n.etags[path] = resp.Header.Get("ETag")
	n.mu.Unlock()
	return body, nil
}

func (n *Node) post(ctx context.Context, path string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.cfg.PanelURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	n.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("%s: invalid response: %w", path, err)
	}
	if result.Code != 0 {
		return fmt.Errorf("%s: code %d: %s", path, result.Code, result.Message)
	}
	return nil
}

func (n *Node) setHeaders(req *http.Request) {
	req.Header.Set("API-KEY", n.cfg.APIKey)
	req.Header.Set("NODE-ID", n.cfg.NodeID)
	req.Header.Set("NODE-TYPE", n.cfg.NodeType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "linkc-node-m0")
}

// --- OS status -------------------------------------------------------------

type cpuSample struct {
	total uint64
	busy  uint64
}

func readCPUSample() cpuSample {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}
	}
	var vals []uint64
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		vals = append(vals, v)
	}
	var total, idle uint64
	for i, v := range vals {
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return cpuSample{total: total, busy: total - idle}
}

func readMemInfo() (total, available, swapTotal, swapFree int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	parse := func(name string) int64 {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, name+":") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					v, _ := strconv.ParseInt(f[1], 10, 64)
					return v * 1024
				}
			}
		}
		return 0
	}
	return parse("MemTotal"), parse("MemAvailable"), parse("SwapTotal"), parse("SwapFree")
}

func readDisk(path string) (total, used int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	avail := int64(st.Bavail) * bs
	return total, total - avail
}

func readUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}
