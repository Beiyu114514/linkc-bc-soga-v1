// Package agent wires the soga-v1 panel, embedded Xray runtime, durable
// traffic journal and status sampler into the node service.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/audit"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/node"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/status"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/traffic"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/version"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// Snapshot is a read-only view for the admin endpoint and tests.
type Snapshot struct {
	Ready         bool              `json:"ready"`
	Degraded      bool              `json:"degraded"`
	NodeID        string            `json:"node_id"`
	Port          int               `json:"port"`
	Users         int               `json:"users"`
	OnlineIPs     int               `json:"online_ips"`
	PendingBytes  int64             `json:"pending_bytes"`
	LimitsMode    string            `json:"limits_mode"`
	NodeSpeed     float64           `json:"node_speed_mbps"`
	ActiveTCP     int               `json:"active_tcp"`
	ActiveDevices int               `json:"active_devices"`
	RejectsTCP    uint64            `json:"rejects_tcp"`
	RejectsDevice uint64            `json:"rejects_device"`
	AuditRules    int               `json:"audit_rules"`
	AuditHits     uint64            `json:"audit_hits"`
	AuditReported uint64            `json:"audit_reported"`
	AuditBlocked  uint64            `json:"audit_blocked"`
	AuditPending  int               `json:"audit_pending"`
	LastError     string            `json:"last_error,omitempty"`
	LastSuccess   map[string]string `json:"last_success"`
	Version       string            `json:"version"`
	StartedAt     time.Time         `json:"started_at"`
}

// Agent is one node process.
type Agent struct {
	cfg        config.Config
	log        *slog.Logger
	store      *store.Store
	panel      *panel.Client
	status     *status.Collector
	audit      *audit.Checker
	alive      *aliveTracker
	localWhite []string

	certs []xray.TLSCertificate

	mu          sync.Mutex
	engine      *xray.Engine
	trafficMgr  *traffic.Manager
	nodeSpec    node.Spec
	xrayCfg     xray.NodeConfig
	inboundFP   string
	users       []xray.User
	usersFP     string
	hasNode     bool
	hasUsers    bool
	auditLoaded bool
	// auditPending records that a fetched rule/whitelist version was not
	// applied yet. The panel client advances its ETag cache on 200, so the
	// next pull answers 304 and would otherwise never retry the update.
	auditPending bool

	pullEvery    time.Duration
	collectEvery time.Duration
	pushEvery    time.Duration
	statusEvery  time.Duration

	ready     bool
	closed    bool
	degraded  bool
	failures  map[string]int
	applyErr  map[string]error
	lastOK    map[string]time.Time
	lastErr   error
	startedAt time.Time

	// stopped is set by shutdown() while holding applyMu. ReloadCerts (SIGHUP)
	// re-checks it after taking applyMu, so a certificate reload can never
	// outlive the Xray engine that shutdown() closes.
	stopped bool

	applyMu   sync.Mutex
	collectMu sync.Mutex

	// pendingRemove holds inbound tags whose removal failed and must be
	// retried, otherwise a stale listener could keep serving old users/certs.
	pendingRemove []string
}

// New opens the store and panel client. Run performs the first bootstrap.
func New(cfg config.Config, logger *slog.Logger) (*Agent, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	if cfg.Panel.UserAgent == "" || cfg.Panel.UserAgent == "linkc-node/dev" {
		cfg.Panel.UserAgent = version.UserAgent()
	}
	st, err := store.Open(cfg.StateDBPath())
	if err != nil {
		return nil, err
	}
	pc, err := panel.New(panel.Options{
		BaseURL:         cfg.Panel.BaseURL,
		APIKey:          cfg.Panel.APIKey,
		NodeID:          cfg.Panel.NodeID,
		NodeType:        cfg.Panel.NodeType,
		Timeout:         cfg.Panel.Timeout.Duration(),
		VerifyTLS:       cfg.Panel.TLS.Verify,
		CAFile:          cfg.Panel.TLS.CAFile,
		ServerName:      cfg.Panel.TLS.ServerName,
		ClientCertFile:  cfg.Panel.TLS.ClientCertFile,
		ClientKeyFile:   cfg.Panel.TLS.ClientKeyFile,
		AllowHTTP:       cfg.Panel.AllowHTTP,
		Proxy:           cfg.Panel.Proxy,
		MaxResponseSize: cfg.Panel.MaxResponseSize,
		MaxAttempts:     cfg.Panel.MaxAttempts,
		UserAgent:       cfg.Panel.UserAgent,
	}, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	certs, err := loadCertMaterials(cfg.Node.TLS.Certificates)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	localWhite, err := loadWhiteList(cfg.Node.Audit.WhiteListFile)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	a := &Agent{
		cfg:          cfg,
		log:          logger,
		store:        st,
		panel:        pc,
		status:       status.New(cfg.Storage.Path),
		audit:        newAuditChecker(st, cfg, logger),
		alive:        newAliveTracker(cfg.Node.AliveIP.FullEvery, cfg.Node.AliveIP.Mode),
		localWhite:   localWhite,
		certs:        certs,
		pullEvery:    cfg.Node.PullInterval.Duration(),
		collectEvery: cfg.Node.CollectInterval.Duration(),
		pushEvery:    cfg.Node.PushInterval.Duration(),
		statusEvery:  cfg.Node.StatusInterval.Duration(),
		failures:     make(map[string]int),
		applyErr:     make(map[string]error),
		lastOK:       make(map[string]time.Time),
		startedAt:    time.Now(),
	}
	return a, nil
}

// Bootstrap starts from last_good if present; otherwise it performs the first
// panel fetch. It is safe to call once before Run.
func (a *Agent) Bootstrap(ctx context.Context) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	var rawNode panel.Node
	haveNode, err := a.store.GetLastGood(a.lastGood("node"), &rawNode)
	if err != nil {
		return err
	}
	var rawUsers []panel.User
	haveUsers, err := a.store.GetLastGood(a.lastGood("users"), &rawUsers)
	if err != nil {
		return err
	}

	var rawRules []panel.AuditRule
	haveRules, err := a.store.GetLastGood(a.lastGood("audit_rules"), &rawRules)
	if err != nil {
		return err
	}
	var rawWhite []string
	haveWhite, err := a.store.GetLastGood(a.lastGood("white_list"), &rawWhite)
	if err != nil {
		return err
	}

	fetchedNode, fetchedUsers := false, false
	if !haveNode {
		n, _, err := a.panel.FetchNode(ctx)
		if err != nil {
			return fmt.Errorf("bootstrap node: %w", err)
		}
		rawNode, haveNode, fetchedNode = n, true, true
	}
	if !haveUsers {
		u, _, err := a.panel.FetchUsers(ctx)
		if err != nil {
			return fmt.Errorf("bootstrap users: %w", err)
		}
		rawUsers, haveUsers, fetchedUsers = u, true, true
	}

	if a.audit != nil {
		if !haveRules {
			if r, _, err := a.panel.FetchAuditRules(ctx); err == nil {
				rawRules, haveRules = r, true
			} else {
				a.log.Warn("bootstrap audit_rules", "error", err)
			}
		}
		if !haveWhite {
			if w, _, err := a.panel.FetchWhiteList(ctx); err == nil {
				rawWhite, haveWhite = w, true
			} else {
				a.log.Warn("bootstrap white_list", "error", err)
			}
		}
		if haveRules || haveWhite {
			if _, err := a.audit.Update(rawRules, mergeWhiteList(rawWhite, a.localWhite)); err != nil {
				a.log.Warn("bootstrap audit compile", "error", err)
			} else {
				// auditLoaded stays false on purpose: the rules above may come
				// from last_good while the ETag cache already holds a newer body
				// (e.g. a restart between a failed pull and its retry). Leaving
				// it false makes the first successful Sync reconcile the audit
				// set instead of trusting last_good forever.
				// Persist only what is actually live, like Sync does: a failed
				// compile must not let last_good lead the applied rules.
				if haveRules {
					if err := a.store.PutLastGood(a.lastGood("audit_rules"), rawRules); err != nil {
						a.log.Warn("save last_good audit_rules", "error", err)
					}
				}
				if haveWhite {
					if err := a.store.PutLastGood(a.lastGood("white_list"), rawWhite); err != nil {
						a.log.Warn("save last_good white_list", "error", err)
					}
				}
			}
		}
	}

	spec, err := node.FromPanel(rawNode)
	if err != nil {
		return fmt.Errorf("bootstrap node: %w", err)
	}
	users, _, err := node.NormalizeUsers(rawUsers, a.userOptions(), nil)
	if err != nil {
		return fmt.Errorf("bootstrap users: %w", err)
	}
	xc, err := spec.Xray(a.localRuntime())
	if err != nil {
		return fmt.Errorf("bootstrap node: %w", err)
	}
	engine, err := xray.Start(ctx, xc, users)
	if err != nil {
		return fmt.Errorf("start xray: %w", err)
	}
	tm := traffic.New(a.store, engine)

	a.mu.Lock()
	a.engine = engine
	a.trafficMgr = tm
	a.nodeSpec = spec
	a.xrayCfg = xc
	a.inboundFP = node.InboundFingerprint(xc)
	a.users = users
	a.usersFP = node.UsersFingerprint(users)
	a.hasNode, a.hasUsers = true, true
	a.ready = true
	a.updateIntervalsLocked(spec)
	a.mu.Unlock()

	a.status.Refresh()
	a.saveNode(spec)
	a.saveUsers(users)
	if fetchedNode || fetchedUsers {
		a.log.Info("bootstrap from panel", "node_id", a.cfg.Panel.NodeID, "users", len(users), "port", spec.Config.Port)
	} else {
		a.log.Info("bootstrap from last_good", "node_id", a.cfg.Panel.NodeID, "users", len(users), "port", spec.Config.Port)
	}
	return nil
}

// Run bootstraps, starts the periodic loops and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Bootstrap(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	start := func(fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(runCtx)
		}()
	}
	start(a.pullLoop)
	start(a.collectLoop)
	start(a.pushLoop)
	start(a.statusLoop)

	<-ctx.Done()
	cancel()
	wg.Wait()
	a.shutdown()
	return nil
}

// Close stops Xray and closes the store. Safe to call after Run.
func (a *Agent) Close() error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.stopped = true
	a.ready = false
	engine := a.engine
	a.engine = nil
	a.mu.Unlock()
	var errs []error
	if engine != nil {
		errs = append(errs, engine.Close())
	}
	errs = append(errs, a.store.Close())
	return errors.Join(errs...)
}

// Snapshot returns the current status.
func (a *Agent) Snapshot() Snapshot {
	a.mu.Lock()
	s := Snapshot{
		Ready: a.ready, Degraded: a.degraded, NodeID: a.cfg.Panel.NodeID,
		Users: len(a.users), LastSuccess: make(map[string]string, len(a.lastOK)),
		LastError: errString(a.lastErr), Version: version.Version, StartedAt: a.startedAt,
	}
	if a.engine != nil {
		s.Port = a.engine.NodePort()
		s.OnlineIPs = len(a.engine.OnlineIPs())
		if gate := a.engine.Gate(); gate != nil {
			m := gate.Metrics()
			s.LimitsMode, s.NodeSpeed = m.Mode, m.NodeSpeed
			s.ActiveTCP, s.ActiveDevices = m.ActiveTCP, m.Devices
			s.RejectsTCP, s.RejectsDevice = m.RejectsTCP, m.RejectsDevice
		}
	}
	for k, v := range a.lastOK {
		s.LastSuccess[k] = v.Format(time.RFC3339)
	}
	closed := a.closed
	tm := a.trafficMgr
	checker := a.audit
	a.mu.Unlock()
	if !closed && tm != nil {
		s.PendingBytes = tm.PendingBytes()
	}
	if !closed && checker != nil {
		st := checker.Stats()
		s.AuditRules = checker.Rules().Len()
		s.AuditHits = st.Hits
		s.AuditReported = st.Reported
		s.AuditBlocked = st.Blocked
		s.AuditPending = checker.Pending()
	}
	return s
}

// Engine exposes the runtime for tests.
func (a *Agent) Engine() *xray.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

func (a *Agent) userOptions() node.UserOptions {
	return node.UserOptions{
		AllowEmpty:      a.cfg.Node.Users.AllowEmpty,
		MassRemoveGuard: a.cfg.Node.Users.MassRemoveGuard,
		MaxUsers:        a.cfg.Node.Users.MaxUsers,
	}
}

func (a *Agent) localRuntime() node.LocalRuntime { return a.localRuntimeWith(a.certs) }

func (a *Agent) localRuntimeWith(certs []xray.TLSCertificate) node.LocalRuntime {
	return node.LocalRuntimeFromConfig(a.cfg, certs, a.auditHook())
}

// LastGoodKey scopes a persisted last-good object to the panel node so that
// changing panel.node_id on the same state DB cannot replay another node's
// cached config.
func LastGoodKey(nodeID, kind string) string { return nodeID + "|" + kind }

func (a *Agent) lastGood(kind string) string { return LastGoodKey(a.cfg.Panel.NodeID, kind) }

// commitNode makes the agent state match a successfully applied inbound.
func (a *Agent) commitNode(spec node.Spec, xc xray.NodeConfig, users []xray.User) {
	a.mu.Lock()
	a.nodeSpec, a.xrayCfg = spec, xc
	a.inboundFP = node.InboundFingerprint(xc)
	a.users, a.usersFP = users, node.UsersFingerprint(users)
	a.hasNode, a.hasUsers = true, true
	a.updateIntervalsLocked(spec)
	a.mu.Unlock()
	a.saveNode(spec)
	a.saveUsers(users)
}

// ReloadCerts re-reads certificate files and hot-swaps the inbound when the
// inline material changed. SIGHUP calls this.
func (a *Agent) ReloadCerts() error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	a.mu.Lock()
	if a.closed || a.stopped {
		// Shutdown already closed the store and engine; nothing to reload.
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	certs, err := loadCertMaterials(a.cfg.Node.TLS.Certificates)
	if err != nil {
		return err
	}
	a.mu.Lock()
	engine := a.engine
	spec := a.nodeSpec
	users := append([]xray.User(nil), a.users...)
	oldFP := a.inboundFP
	a.mu.Unlock()
	if engine == nil {
		a.certs = certs
		return nil
	}
	a.flushPendingRemove(context.Background(), engine)
	if sameCerts(a.certs, certs) {
		return nil
	}
	xc, err := spec.Xray(a.localRuntimeWith(certs))
	if err != nil {
		return err
	}
	newFP := node.InboundFingerprint(xc)
	if newFP == oldFP {
		a.certs = certs
		return nil
	}
	a.collect(context.Background())
	rep, err := engine.SwapInbound(context.Background(), xc, users)
	if err != nil {
		if rep.StaleTag != "" {
			a.log.Error("reload certs: old inbound still bound; queued for retry", "tag", rep.StaleTag, "error", err)
			a.queueRemove(rep.StaleTag)
		}
		if !rep.Applied {
			if rep.Tag == "" {
				// No listener: force the next pull to re-apply the node config.
				a.mu.Lock()
				a.inboundFP = ""
				a.mu.Unlock()
			}
			a.collect(context.Background())
			return fmt.Errorf("reload certs: %w", err)
		}
		// The new inbound is live (possibly with a stale old listener); keep
		// the new certificate material and surface the warning below.
		a.log.Warn("certificates reloaded with warning", "warning", rep.Warning, "error", err)
	}
	a.certs = certs
	a.mu.Lock()
	a.xrayCfg = xc
	a.inboundFP = newFP
	a.mu.Unlock()
	a.collect(context.Background())
	if err != nil {
		return err
	}
	if rep.Warning != "" {
		a.log.Warn("certificates reloaded with warning", "warning", rep.Warning)
	}
	a.log.Info("certificates reloaded", "certificates", len(certs))
	return nil
}

func sameCerts(a, b []xray.TLSCertificate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].CertPEM != b[i].CertPEM || a[i].KeyPEM != b[i].KeyPEM {
			return false
		}
	}
	return true
}

const maxRemoveRetries = 3

func isNotFound(err error) bool {
	return err == nil || strings.Contains(strings.ToLower(err.Error()), "not found")
}

// queueRemove remembers an inbound tag whose removal failed so a later pull
// can retry it instead of leaving a listener with stale users/certs behind.
func (a *Agent) queueRemove(tag string) {
	if tag == "" {
		return
	}
	a.mu.Lock()
	a.pendingRemove = append(a.pendingRemove, tag)
	a.mu.Unlock()
}

// removeInbound removes an inbound, retrying transient failures. If removal
// keeps failing the tag is queued so the stale listener is cleaned up later
// instead of silently serving old users/certs.
func (a *Agent) removeInbound(ctx context.Context, engine *xray.Engine, tag string) error {
	var err error
	for attempt := 0; attempt < maxRemoveRetries; attempt++ {
		if err = engine.RemoveInbound(ctx, tag); isNotFound(err) {
			return nil
		}
		if attempt < maxRemoveRetries-1 {
			if !sleepCtx(ctx, time.Duration(100*(attempt+1))*time.Millisecond) {
				break
			}
		}
	}
	a.queueRemove(tag)
	return err
}

// flushPendingRemove retries queued inbound removals.
func (a *Agent) flushPendingRemove(ctx context.Context, engine *xray.Engine) {
	a.mu.Lock()
	pending := a.pendingRemove
	a.pendingRemove = nil
	a.mu.Unlock()
	var keep []string
	for _, tag := range pending {
		if tag == "" {
			continue
		}
		if err := engine.RemoveInbound(ctx, tag); !isNotFound(err) {
			keep = append(keep, tag)
		}
	}
	if len(keep) > 0 {
		a.mu.Lock()
		a.pendingRemove = append(a.pendingRemove, keep...)
		a.mu.Unlock()
	}
}

// auditHook returns a nil interface when audit is disabled, avoiding a typed
// nil that would pass a != nil check in the gatekeeper.
func (a *Agent) auditHook() xray.AuditChecker {
	if a.audit == nil {
		return nil
	}
	return a.audit
}

func (a *Agent) updateIntervalsLocked(spec node.Spec) {
	if spec.Basic.PullInterval >= 1 {
		a.pullEvery = time.Duration(spec.Basic.PullInterval) * time.Second
	}
	if spec.Basic.PushInterval >= 1 {
		a.pushEvery = time.Duration(spec.Basic.PushInterval) * time.Second
	}
}

func (a *Agent) saveNode(spec node.Spec) {
	pn := a.specPanel(spec)
	if err := a.store.PutLastGood(a.lastGood("node"), pn); err != nil {
		a.log.Warn("save last_good node", "error", err)
	}
}

func (a *Agent) saveUsers(users []xray.User) {
	pu := make([]panel.User, 0, len(users))
	for _, u := range users {
		pu = append(pu, panel.User{ID: u.ID, UUID: u.UUID, SpeedLimit: u.SpeedLimit, DeviceLimit: u.DeviceLimit, TCPLimit: u.TCPLimit})
	}
	if err := a.store.PutLastGood(a.lastGood("users"), pu); err != nil {
		a.log.Warn("save last_good users", "error", err)
	}
}

func (a *Agent) recordSuccess(endpoint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[endpoint] = 0
	a.lastOK[endpoint] = time.Now()
	a.recomputeLastErrLocked()
	a.updateDegradedLocked()
}

func (a *Agent) recordFailure(endpoint string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[endpoint]++
	if err != nil {
		a.lastErr = err
	}
	a.updateDegradedLocked()
}

func (a *Agent) setApplyError(kind string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.applyErr == nil {
		a.applyErr = make(map[string]error)
	}
	if err == nil {
		delete(a.applyErr, kind)
		a.recomputeLastErrLocked()
		return
	}
	a.applyErr[kind] = err
	a.lastErr = err
}

func (a *Agent) recomputeLastErrLocked() {
	for _, n := range a.failures {
		if n > 0 {
			return
		}
	}
	if len(a.applyErr) == 0 {
		a.lastErr = nil
		return
	}
	keys := make([]string, 0, len(a.applyErr))
	for k := range a.applyErr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a.lastErr = a.applyErr[keys[0]]
}

// setAuditPending marks whether a fetched audit rule set still has to be
// applied. See the auditPending field for why this is needed.
func (a *Agent) setAuditPending(pending bool) {
	a.mu.Lock()
	a.auditPending = pending
	a.mu.Unlock()
}

func (a *Agent) updateDegradedLocked() {
	a.degraded = a.failures["node"] >= 3 || a.failures["users"] >= 3
}

func (a *Agent) interval(kind string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch kind {
	case "pull":
		return a.pullEvery
	case "collect":
		return a.collectEvery
	case "push":
		return a.pushEvery
	case "status":
		return a.statusEvery
	}
	return time.Minute
}

func (a *Agent) shutdown() {
	// Run() returns only after this function, so the SIGHUP goroutine in
	// cmd/linkc-node is joined *after* the engine is already closed. Serialize
	// with ReloadCerts instead: it holds applyMu for its whole run and
	// re-checks the stop flag once it has the lock, so the engine is never
	// closed underneath an in-flight inbound swap.
	a.applyMu.Lock()
	a.mu.Lock()
	a.stopped = true
	engine := a.engine
	a.engine = nil
	a.ready = false
	a.mu.Unlock()
	defer a.applyMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Flush online IPs before closing Xray (Close tears down OnlineMaps).
	if engine != nil {
		if err := a.pushAliveWithEngine(ctx, engine); err != nil {
			a.log.Warn("final alive_ip push", "error", err)
		}
		if err := engine.Close(); err != nil {
			a.log.Warn("close xray", "error", err)
		}
	}
	// Collect and push after engine.Close() so final bytes on closing
	// connections are included before the process exits.
	a.collect(ctx)
	if err := a.Push(ctx); err != nil {
		a.log.Warn("final push", "error", err)
	}
}

func loadCertMaterials(certs []config.CertPair) ([]xray.TLSCertificate, error) {
	out := make([]xray.TLSCertificate, 0, len(certs))
	for _, cp := range certs {
		b, err := os.ReadFile(cp.CertFile)
		if err != nil {
			return nil, fmt.Errorf("read certificate %s: %w", cp.CertFile, err)
		}
		k, err := os.ReadFile(cp.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", cp.KeyFile, err)
		}
		out = append(out, xray.TLSCertificate{CertPEM: string(b), KeyPEM: string(k)})
	}
	return out, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// jitter returns d +/- 10%.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	f := 0.9 + rand.Float64()*0.2
	return time.Duration(float64(d) * f)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (a *Agent) pullLoop(ctx context.Context) {
	if err := a.Sync(ctx); err != nil {
		a.log.Warn("panel sync", "error", err)
	}
	for {
		if !sleepCtx(ctx, jitter(a.interval("pull"))) {
			return
		}
		if err := a.Sync(ctx); err != nil {
			a.log.Warn("panel sync", "error", err)
		}
	}
}

func (a *Agent) collectLoop(ctx context.Context) {
	for {
		if !sleepCtx(ctx, jitter(a.interval("collect"))) {
			return
		}
		a.collect(ctx)
	}
}

func (a *Agent) pushLoop(ctx context.Context) {
	if err := a.Push(ctx); err != nil {
		a.log.Warn("panel push", "error", err)
	}
	for {
		if !sleepCtx(ctx, jitter(a.interval("push"))) {
			return
		}
		if err := a.Push(ctx); err != nil {
			a.log.Warn("panel push", "error", err)
		}
	}
}

func (a *Agent) statusLoop(ctx context.Context) {
	a.status.Refresh()
	for {
		if !sleepCtx(ctx, jitter(a.interval("status"))) {
			return
		}
		a.status.Refresh()
	}
}

// collect snapshots counters and journals them; it never blocks config apply
// longer than a journal fsync.
func (a *Agent) collect(ctx context.Context) {
	a.collectMu.Lock()
	defer a.collectMu.Unlock()
	a.mu.Lock()
	tm := a.trafficMgr
	a.mu.Unlock()
	if tm == nil {
		return
	}
	// Flush the in-memory backlog first: the size guard must only look at the
	// durable journal. Counting mem here used to freeze the pipeline forever
	// (mem kept the guard tripped, so Collect never flushed it, and Push
	// refused to drain the journal while mem was non-empty).
	if err := tm.FlushMem(); err != nil {
		a.log.Error("flush traffic backlog", "error", err, "pending_bytes", tm.PendingBytes())
	}
	if max := a.cfg.Storage.MaxJournalBytes; max > 0 && tm.JournalBytes() > max {
		a.log.Error("traffic journal is full; collection paused", "journal_bytes", tm.JournalBytes(), "pending_bytes", tm.PendingBytes())
		return
	}
	if n, err := tm.Collect(); err != nil {
		a.log.Error("collect traffic", "error", err, "bytes", n)
	}
}

// Push sends pending traffic and a status sample.
func (a *Agent) Push(ctx context.Context) error {
	a.mu.Lock()
	tm := a.trafficMgr
	a.mu.Unlock()
	var firstErr error
	if tm != nil {
		if pending, journal := tm.PendingBytes(), tm.JournalBytes(); pending > journal {
			// Not fatal: unjournaled deltas are newer than the journal and do
			// not conflict with pushing the durable records.
			a.log.Warn("traffic backlog is not journaled yet", "pending_bytes", pending, "journal_bytes", journal)
		}
		if n, err := tm.Push(ctx, a.panel, a.cfg.Node.Traffic.BatchUsers, a.cfg.Node.Traffic.BatchBytes); err != nil {
			firstErr = err
			a.setApplyError("traffic", err)
		} else {
			a.setApplyError("traffic", nil)
			if n > 0 {
				a.log.Debug("traffic pushed", "bytes", n)
			}
		}
	}
	if a.cfg.Node.Traffic.IncludeOnlineZero {
		a.mu.Lock()
		engine := a.engine
		a.mu.Unlock()
		if engine != nil {
			var zeros []panel.Traffic
			for id := range engine.OnlineIPs() {
				zeros = append(zeros, panel.Traffic{ID: id, U: 0, D: 0})
			}
			batchUsers := a.cfg.Node.Traffic.BatchUsers
			if batchUsers <= 0 {
				batchUsers = 500
			}
			for start := 0; start < len(zeros); start += batchUsers {
				end := start + batchUsers
				if end > len(zeros) {
					end = len(zeros)
				}
				if err := a.panel.PushTraffic(ctx, zeros[start:end]); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("traffic online-zero: %w", err)
					}
					a.setApplyError("traffic", err)
					break
				}
			}
		}
	}
	if err := a.pushAliveIP(ctx); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("alive_ip: %w", err)
		}
		a.recordFailure("alive_ip", err)
	} else {
		a.recordSuccess("alive_ip")
	}
	if a.audit != nil {
		if _, err := a.audit.Push(ctx, a.panel, 500); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("audit_log: %w", err)
			}
			a.recordFailure("audit_log", err)
		} else {
			a.recordSuccess("audit_log")
		}
	}

	st := a.status.Snapshot()
	if err := a.panel.PushStatus(ctx, panel.Status{
		CPUPercent: st.CPUPercent, MemTotal: st.MemTotal, MemUsed: st.MemUsed,
		SwapTotal: st.SwapTotal, SwapUsed: st.SwapUsed,
		DiskTotal: st.DiskTotal, DiskUsed: st.DiskUsed, Uptime: st.Uptime,
	}, a.cfg.Node.Status.Mode); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("status: %w", err)
		}
		a.recordFailure("status", err)
	} else {
		a.recordSuccess("status")
	}
	return firstErr
}

// OnlineIPs returns the current Xray OnlineMap snapshot (for M2 alive_ip).
func (a *Agent) OnlineIPs() map[int64][]string {
	a.mu.Lock()
	e := a.engine
	a.mu.Unlock()
	if e == nil {
		return nil
	}
	return e.OnlineIPs()
}

func newAuditChecker(st *store.Store, cfg config.Config, log *slog.Logger) *audit.Checker {
	if !cfg.Node.Audit.Enabled {
		return nil
	}
	return audit.New(st, audit.Config{
		Mode:          cfg.Node.Audit.RuleMode,
		Action:        cfg.Node.Audit.Action,
		Strict:        cfg.Node.Audit.StrictRules,
		DedupeTTL:     cfg.Node.Audit.DedupeTTL.Duration(),
		GeodataDir:    cfg.Node.Geodata.Dir,
		ResolveDomain: cfg.Node.Audit.ResolveDomain,
	}, log)
}

// mergeWhiteList combines the panel list with the effective local whitelist
// file contents.
func mergeWhiteList(fromPanel, local []string) []string {
	out := append([]string(nil), fromPanel...)
	return append(out, local...)
}

// loadWhiteList reads a newline-separated whitelist file (“#“ comments and
// blank lines are ignored).
func loadWhiteList(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read whitelist: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}
