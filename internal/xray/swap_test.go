package xray

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xtls/xray-core/features/inbound"

	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	xproxy "golang.org/x/net/proxy"
)

const (
	swapTestUUID  = "701534e1-8540-49f3-87af-71680a1b97ff"
	swapTestUUID2 = "0f0f0f0f-0000-4000-8000-000000000002"
)

// flakyInboundMgr wraps the real inbound manager with injectable remove and
// get failures. Adds cannot be intercepted here (Xray resolves the manager
// from the instance), so add failures are injected via the addInbound seam.
type flakyInboundMgr struct {
	inbound.Manager
	failRemove func(tag string) bool
	failGet    func(tag string) bool
}

func (m *flakyInboundMgr) RemoveHandler(ctx context.Context, tag string) error {
	if m.failRemove != nil && m.failRemove(tag) {
		return fmt.Errorf("injected remove failure for %s", tag)
	}
	return m.Manager.RemoveHandler(ctx, tag)
}

func (m *flakyInboundMgr) GetHandler(ctx context.Context, tag string) (inbound.Handler, error) {
	if m.failGet != nil && m.failGet(tag) {
		return nil, fmt.Errorf("injected get failure for %s", tag)
	}
	return m.Manager.GetHandler(ctx, tag)
}

func swapTestNode(port int) NodeConfig {
	return NodeConfig{Listen: "127.0.0.1", Port: port, StreamType: "tcp", TLSType: "none", AllowPrivate: true}
}

func swapTestUsers(ids ...int64) []User {
	out := make([]User, 0, len(ids))
	for _, id := range ids {
		uuid := swapTestUUID
		if id == 2 {
			uuid = swapTestUUID2
		}
		out = append(out, User{ID: id, UUID: uuid})
	}
	return out
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func swapTestClient(t *testing.T, ctx context.Context, port int, uuid string) *http.Client {
	t.Helper()
	socksPort := freeTestPort(t)
	inst, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: uuid,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	d, err := xproxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		t.Fatal("socks dialer not context aware")
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: cd.DialContext}}
}

// failRemovesFor makes the engine's inbound manager reject RemoveHandler for
// the given tags.
func failRemovesFor(e *Engine, tags ...string) {
	set := make(map[string]bool, len(tags))
	for _, tag := range tags {
		set[tag] = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inboundMgr = &flakyInboundMgr{Manager: e.inboundMgr, failRemove: func(tag string) bool { return set[tag] }}
}

// failAllRemoves makes every RemoveHandler fail (used for the "stuck
// listener" case, where the rollback cannot drop the new inbound either).
func failAllRemoves(e *Engine) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inboundMgr = &flakyInboundMgr{Manager: e.inboundMgr, failRemove: func(string) bool { return true }}
}

// restoreManager puts the real Xray inbound manager back.
func restoreManager(e *Engine) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inboundMgr = e.inst.GetFeature(inbound.ManagerType()).(inbound.Manager)
}

// failAddsAfter makes the next `n` adds succeed and every later add fail.
// The returned function disarms the injection.
func failAddsAfter(t *testing.T, n int) func() {
	t.Helper()
	orig := addInbound
	adds := 0
	addInbound = func(e *Engine, ctx context.Context, node NodeConfig, users []User, tag string) error {
		if adds >= n {
			return fmt.Errorf("injected add failure")
		}
		adds++
		return orig(e, ctx, node, users, tag)
	}
	return func() { addInbound = orig }
}

func TestSwapInboundRollbackRestoresOld(t *testing.T) {
	ctx := context.Background()
	oldPort, newPort := freeTestPort(t), freeTestPort(t)
	e, err := Start(ctx, swapTestNode(oldPort), swapTestUsers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oldTag := e.Tag()
	hc := swapTestClient(t, ctx, oldPort, swapTestUUID)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "before-swap")
	}))
	defer target.Close()
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("pre-swap request: %v", err)
	} else {
		resp.Body.Close()
	}

	// The old listener cannot be removed: the swap must roll back to the
	// previous config instead of leaving a stale inbound behind.
	failRemovesFor(e, oldTag)

	rep, err := e.SwapInbound(ctx, swapTestNode(newPort), swapTestUsers(1, 2))
	if err == nil {
		t.Fatal("expected swap error when the old listener cannot be removed")
	}
	if !rep.RolledBack || rep.Applied {
		t.Fatalf("expected a non-applied rollback, got %+v (err=%v)", rep, err)
	}
	if rep.Tag != oldTag || e.Tag() != oldTag {
		t.Fatalf("tag = %q / engine %q, want %q", rep.Tag, e.Tag(), oldTag)
	}
	if got := e.NodePort(); got != oldPort {
		t.Fatalf("engine port = %d, want %d", got, oldPort)
	}
	if !portOpen(oldPort) {
		t.Fatal("old inbound is not listening after rollback")
	}
	if portOpen(newPort) {
		t.Fatal("new inbound still listening after rollback")
	}
	// The rolled-back inbound must still serve the original user set.
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("request after rollback: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "before-swap" {
		t.Fatalf("body = %q", body)
	}
	// User 2 was only ever part of the discarded new config.
	hc2 := swapTestClient(t, ctx, oldPort, swapTestUUID2)
	if resp, err := hc2.Get(target.URL); err == nil {
		resp.Body.Close()
		t.Fatal("user 2 must not authenticate on the rolled-back inbound")
	}

	// A later swap without faults must succeed (self-healing).
	restoreManager(e)
	rep, err = e.SwapInbound(ctx, swapTestNode(newPort), swapTestUsers(1))
	if err != nil {
		t.Fatalf("swap after rollback: %v", err)
	}
	if rep.RolledBack || rep.Tag == oldTag {
		t.Fatalf("expected clean swap, got %+v", rep)
	}
	if !portOpen(newPort) {
		t.Fatal("new inbound not listening after clean swap")
	}
	hc3 := swapTestClient(t, ctx, newPort, swapTestUUID)
	if resp, err := hc3.Get(target.URL); err != nil {
		t.Fatalf("request after clean swap: %v", err)
	} else {
		resp.Body.Close()
	}
}

func TestSwapInboundAddFailureKeepsOld(t *testing.T) {
	ctx := context.Background()
	oldPort, newPort := freeTestPort(t), freeTestPort(t)
	e, err := Start(ctx, swapTestNode(oldPort), swapTestUsers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oldTag := e.Tag()
	hc := swapTestClient(t, ctx, oldPort, swapTestUUID)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "before-swap")
	}))
	defer target.Close()
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("pre-swap request: %v", err)
	} else {
		resp.Body.Close()
	}

	disarm := failAddsAfter(t, 0)
	defer disarm()

	rep, err := e.SwapInbound(ctx, swapTestNode(newPort), swapTestUsers(1))
	if err == nil {
		t.Fatal("expected add failure")
	}
	if rep.RolledBack || rep.Applied {
		t.Fatalf("add failure must not report applied/rolled back: %+v", rep)
	}
	if rep.Tag != oldTag || e.Tag() != oldTag {
		t.Fatalf("tag = %q / engine %q, want the still-active %q", rep.Tag, e.Tag(), oldTag)
	}
	if !portOpen(oldPort) {
		t.Fatal("old inbound must keep listening")
	}
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("request after failed swap: %v", err)
	}
	resp.Body.Close()
}

func TestSwapInboundKeepsNewWhenOldRemovalStuck(t *testing.T) {
	ctx := context.Background()
	oldPort, newPort := freeTestPort(t), freeTestPort(t)
	e, err := Start(ctx, swapTestNode(oldPort), swapTestUsers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oldTag := e.Tag()

	// No inbound can be removed at all: the rollback cannot drop the new
	// listener either, so the new (live) inbound is kept.
	failAllRemoves(e)

	rep, err := e.SwapInbound(ctx, swapTestNode(newPort), swapTestUsers(1))
	if err == nil {
		t.Fatal("expected error for the stuck old listener")
	}
	if rep.RolledBack || !rep.Applied {
		t.Fatalf("expected an applied swap with a stale old listener: %+v", rep)
	}
	if rep.Warning == "" {
		t.Fatal("expected a warning about the stale old listener")
	}
	if rep.StaleTag != oldTag {
		t.Fatalf("stale tag = %q, want %q", rep.StaleTag, oldTag)
	}
	if rep.Tag == "" || rep.Tag == oldTag || e.Tag() == oldTag {
		t.Fatalf("new inbound must stay in effect, got tag %q / engine %q", rep.Tag, e.Tag())
	}
	if got := e.NodePort(); got != newPort {
		t.Fatalf("engine port = %d, want %d", got, newPort)
	}
	if !portOpen(newPort) {
		t.Fatal("new inbound must keep serving")
	}
}

func TestSwapInboundOldTagGoneCommitsNew(t *testing.T) {
	ctx := context.Background()
	oldPort, newPort := freeTestPort(t), freeTestPort(t)
	e, err := Start(ctx, swapTestNode(oldPort), swapTestUsers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oldTag := e.Tag()

	// Remove the old handler behind the engine's back: RemoveHandler now
	// returns "not found", which must be treated as "old is already gone"
	// and the new inbound must be committed.
	if err := e.RemoveInbound(ctx, oldTag); err != nil {
		t.Fatal(err)
	}

	rep, err := e.SwapInbound(ctx, swapTestNode(newPort), swapTestUsers(1))
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !rep.Applied || rep.RolledBack {
		t.Fatalf("expected applied swap, got %+v", rep)
	}
	if rep.Tag == "" || rep.Tag == oldTag || e.Tag() != rep.Tag {
		t.Fatalf("new tag = %q / engine %q, want a new tag", rep.Tag, e.Tag())
	}
	if rep.Warning == "" {
		t.Fatal("expected a warning that the old inbound was already gone")
	}
	if got := e.NodePort(); got != newPort {
		t.Fatalf("engine port = %d, want %d", got, newPort)
	}
	if !portOpen(newPort) {
		t.Fatal("new inbound must be listening")
	}

	hc := swapTestClient(t, ctx, newPort, swapTestUUID)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "new-after-gone")
	}))
	defer target.Close()
	resp, err := hc.Get(target.URL)
	if err != nil {
		t.Fatalf("request after swap: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "new-after-gone" {
		t.Fatalf("body = %q", body)
	}
}

func TestSwapInboundValidationErrorKeepsOldTag(t *testing.T) {
	ctx := context.Background()
	oldPort := freeTestPort(t)
	e, err := Start(ctx, swapTestNode(oldPort), swapTestUsers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oldTag := e.Tag()

	bad := swapTestNode(oldPort)
	bad.Port = 0
	rep, err := e.SwapInbound(ctx, bad, swapTestUsers(1))
	if err == nil {
		t.Fatal("expected validation error")
	}
	if rep.Applied || rep.RolledBack {
		t.Fatalf("must not report applied/rolled back: %+v", rep)
	}
	if rep.Tag != oldTag || e.Tag() != oldTag {
		t.Fatalf("tag = %q / engine %q, want the still-active %q", rep.Tag, e.Tag(), oldTag)
	}
	if !portOpen(oldPort) {
		t.Fatal("old inbound must keep listening")
	}
}
