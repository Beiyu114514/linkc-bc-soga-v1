package agent_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
)

// TestSyncAddFailureKeepsOldAndRetries is the regression test for the
// add-failure state corruption: a failed inbound add must not commit the new
// spec/users/fingerprint, and the next pull must retry once the port is free.
func TestSyncAddFailureKeepsOldAndRetries(t *testing.T) {
	portA := freePort(t)
	p, srv := startPanel(t, portA)
	defer srv.Close()
	cfg := testConfig(t, srv.URL, t.TempDir())
	a, stop, _ := startAgent(t, cfg)
	defer stop()
	ctx := context.Background()
	_, hc := startVLESSClient(t, ctx, portA)
	target := bigTarget(t, 1024)
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("initial request: %v", err)
	} else {
		resp.Body.Close()
	}

	// Block the new port with a plain listener (no SO_REUSEPORT): Xray's
	// add must fail with EADDRINUSE.
	portB := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", portB))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	nc := p.Snapshot().Config
	nc.Port = portB
	p.SetNodeConfig(nc)
	p.SetUsers(panelUser(), mockpanel.User{
		ID: 2, UUID: "0f0f0f0f-0000-4000-8000-000000000002",
		TransferEnable: 1 << 40, Enable: true, ExpireIn: time.Now().Add(time.Hour),
	})

	waitFor(t, "apply failure", func() bool { return a.Snapshot().LastError != "" })
	s := a.Snapshot()
	if s.Port != portA {
		t.Fatalf("engine port = %d, want the old port %d", s.Port, portA)
	}
	if s.Users != 2 {
		t.Fatalf("snapshot users = %d, want 2", s.Users)
	}
	// The old inbound must keep serving, and the new user list is applied to
	// it independently of the failed node-config swap.
	if resp, err := hc.Get(target.URL); err != nil {
		t.Fatalf("old inbound must keep serving: %v", err)
	} else {
		resp.Body.Close()
	}
	hcOld2 := startVLESSClientUUID(t, ctx, portA, "0f0f0f0f-0000-4000-8000-000000000002")
	if resp, err := hcOld2.Get(target.URL); err != nil {
		t.Fatalf("user 2 must be applied to the old inbound: %v", err)
	} else {
		resp.Body.Close()
	}

	// Unblock the port; the agent must retry on a later pull and move over.
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "port switch retry", func() bool { return a.Snapshot().Port == portB })
	waitFor(t, "users applied", func() bool { return a.Snapshot().Users == 2 })
	if !portOpenAgent(portB) {
		t.Fatal("new port is not listening after retry")
	}

	// User 2 must authenticate on the new inbound.
	hc2 := startVLESSClientUUID(t, ctx, portB, "0f0f0f0f-0000-4000-8000-000000000002")
	if resp, err := hc2.Get(target.URL); err != nil {
		t.Fatalf("request as user 2 after retry: %v", err)
	} else {
		resp.Body.Close()
	}
}

func startVLESSClientUUID(t *testing.T, ctx context.Context, port int, uuid string) *http.Client {
	t.Helper()
	socksPort := freePort(t)
	inst, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: uuid,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return socksHTTPClient(t, socksPort)
}

func portOpenAgent(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
