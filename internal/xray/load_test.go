package xray_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// loadUsers builds n users with deterministic UUIDs.
func loadUsers(n int) []xray.User {
	out := make([]xray.User, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, xray.User{
			ID:   int64(i),
			UUID: fmt.Sprintf("%08x-0000-4000-8000-%012d", i, i),
		})
	}
	return out
}

// TestEngineTenThousandUsers applies 10k users (the load target from the
// plan) and verifies a real client can authenticate afterwards.
func TestEngineTenThousandUsers(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	users := loadUsers(10000)
	e, err := xray.Start(ctx, baseNode(port), users[:1])
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	start := time.Now()
	if err := e.ApplyUsers(ctx, users); err != nil {
		t.Fatalf("apply 10k users: %v", err)
	}
	apply := time.Since(start)
	t.Logf("ApplyUsers(10000) = %v", apply)
	if apply > 30*time.Second {
		t.Fatalf("ApplyUsers too slow: %v", apply)
	}

	start = time.Now()
	_ = e.ReadTraffic()
	t.Logf("ReadTraffic(10000 users) = %v", time.Since(start))

	start = time.Now()
	_ = e.OnlineIPs()
	t.Logf("OnlineIPs(10000 users) = %v", time.Since(start))

	socksPort := freePort(t)
	client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: users[9999].UUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "10k-ok")
	}))
	defer target.Close()
	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatalf("request as user 10000: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "10k-ok" {
		t.Fatalf("body = %q", body)
	}
}

func BenchmarkEngineApplyUsers10k(b *testing.B) {
	ctx := context.Background()
	port := freeBenchPort(b)
	users := loadUsers(10000)
	e, err := xray.Start(ctx, baseNode(port), users[:1])
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.ApplyUsers(ctx, users); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEngineReadTraffic10k(b *testing.B) {
	ctx := context.Background()
	port := freeBenchPort(b)
	users := loadUsers(10000)
	e, err := xray.Start(ctx, baseNode(port), users)
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.ReadTraffic()
	}
}

// TestEngineThroughputLoopback is the throughput baseline for the load plan:
// one client connection through the embedded node with limits off.
func TestEngineThroughputLoopback(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	e, err := xray.Start(ctx, baseNode(port), []xray.User{{ID: 1, UUID: testUUID}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	socksPort := freePort(t)
	client, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const size = 64 << 20 // 64 MiB
	payload := make([]byte, size)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer target.Close()

	start := time.Now()
	resp, err := socksClient(t, socksPort).Get(target.URL)
	if err != nil {
		t.Fatalf("throughput request: %v", err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	mbps := float64(n) * 8 / 1e6 / elapsed.Seconds()
	t.Logf("throughput: %d bytes in %v = %.1f Mbps", n, elapsed, mbps)
	if n != size {
		t.Fatalf("short read: %d", n)
	}
	// Loopback floor; real WAN throughput is bounded by the node's link.
	if mbps < 100 {
		t.Fatalf("throughput too low: %.1f Mbps", mbps)
	}
	tr := e.ReadTraffic()
	if tr[1].Down < size {
		t.Fatalf("traffic counter = %d, want >= %d", tr[1].Down, size)
	}
}

func freeBenchPort(b *testing.B) int {
	b.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
