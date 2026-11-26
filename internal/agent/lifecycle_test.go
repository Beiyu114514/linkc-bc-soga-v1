package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/mockpanel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// TestReloadCertsAfterShutdownIsNoop guards the SIGHUP/shutdown race: Run()
// closes the Xray engine in shutdown() before cmd/linkc-node joins the reload
// goroutine, so a late ReloadCerts must be a no-op instead of swapping an
// inbound on a closed engine.
func TestReloadCertsAfterShutdownIsNoop(t *testing.T) {
	cfg := config.Default()
	cfg.Panel.BaseURL = "http://127.0.0.1:1"
	cfg.Panel.APIKey = "test-key"
	cfg.Panel.NodeID = "7"
	cfg.Panel.AllowHTTP = true
	cfg.Panel.MaxAttempts = 1
	cfg.Panel.Timeout = config.Duration(50 * time.Millisecond)
	cfg.Node.Limits.Mode = "off"
	cfg.Storage.Path = t.TempDir()
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// New() validates certificates up front, so install a broken path after
	// construction: ReloadCerts would fail loudly if it were not skipped.
	a.cfg.Node.TLS.Certificates = []config.CertPair{{
		CertFile: filepath.Join(t.TempDir(), "missing.pem"),
		KeyFile:  filepath.Join(t.TempDir(), "missing.key"),
	}}

	a.shutdown()
	if err := a.ReloadCerts(); err != nil {
		t.Fatalf("ReloadCerts after shutdown must be a no-op: %v", err)
	}
}

func TestShutdownPushesIncludeOnlineZero(t *testing.T) {
	var mu sync.Mutex
	var pushed []panel.Traffic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/traffic":
			var body []panel.Traffic
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				mu.Lock()
				pushed = append(pushed, body...)
				mu.Unlock()
			}
			_, _ = io.WriteString(w, `{"code":0,"data":true}`)
		default:
			_, _ = io.WriteString(w, `{"code":0,"data":true}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Panel.BaseURL = srv.URL
	cfg.Panel.APIKey = "test-key"
	cfg.Panel.NodeID = "7"
	cfg.Panel.AllowHTTP = true
	cfg.Panel.MaxAttempts = 1
	cfg.Node.Limits.Mode = "off"
	cfg.Node.Traffic.IncludeOnlineZero = true
	cfg.Storage.Path = t.TempDir()

	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	port := testFreePort(t)
	e, err := xray.Start(context.Background(), xray.NodeConfig{
		Listen: "127.0.0.1", Port: port, StreamType: "tcp", TLSType: "none", LimitsMode: "off",
	}, []xray.User{{ID: 1, UUID: "11111111-1111-4111-8111-111111111111"}})
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.engine = e
	a.mu.Unlock()
	a.onlineIPsFn = func(*xray.Engine) map[int64][]string {
		return map[int64][]string{1: {"198.51.100.7"}}
	}

	a.shutdown()

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, tr := range pushed {
		if tr.ID == 1 && tr.U == 0 && tr.D == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("shutdown did not push include_online_zero for online user, pushed=%+v", pushed)
	}
}

func TestSyncSwapWarningClearsUsersApplyError(t *testing.T) {
	portA := testFreePort(t)
	p := mockpanel.New(mockpanel.Config{
		APIKey: "k", NodeID: 1, NodeType: "vless",
		Port: portA, StreamType: "tcp", TLSType: "none", TrafficRate: 1,
		PullInterval: -1, PushInterval: -1,
	})
	p.SetUsers(mockpanel.User{
		ID: 1, UUID: "11111111-1111-4111-8111-111111111111",
		TransferEnable: 1 << 40, Enable: true, ExpireIn: time.Now().Add(time.Hour),
	})
	srv := httptest.NewServer(p)
	defer srv.Close()

	cfg := config.Default()
	cfg.Panel.BaseURL = srv.URL
	cfg.Panel.APIKey = "k"
	cfg.Panel.NodeID = "1"
	cfg.Panel.AllowHTTP = true
	cfg.Panel.MaxAttempts = 1
	cfg.Node.Limits.Mode = "off"
	cfg.Storage.Path = t.TempDir()

	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	a.setApplyError("users", errors.New("stale user apply failure"))
	a.swapInboundFn = func(e *xray.Engine, ctx context.Context, xc xray.NodeConfig, users []xray.User) (xray.SwapReport, error) {
		rep, err := e.SwapInbound(ctx, xc, users)
		if err != nil {
			return rep, err
		}
		return xray.SwapReport{Applied: true, StaleTag: "vless-1"}, errors.New("simulated stale old tag removal failure")
	}

	p.SetUsers(
		mockpanel.User{ID: 1, UUID: "11111111-1111-4111-8111-111111111111", TransferEnable: 1 << 40, Enable: true, ExpireIn: time.Now().Add(time.Hour)},
		mockpanel.User{ID: 2, UUID: "22222222-2222-4222-8222-222222222222", TransferEnable: 1 << 40, Enable: true, ExpireIn: time.Now().Add(time.Hour)},
	)
	nc := p.Snapshot().Config
	nc.Port = testFreePort(t)
	p.SetNodeConfig(nc)
	if err := a.Sync(context.Background()); err == nil {
		t.Fatal("expected Sync to return stale-listener warning error")
	}
	a.mu.Lock()
	userErr := a.applyErr["users"]
	nodeErr := a.applyErr["node"]
	a.mu.Unlock()
	if userErr != nil {
		t.Fatalf("applyErr[users] = %v, want nil", userErr)
	}
	if nodeErr == nil {
		t.Fatal("applyErr[node] = nil, want stale-listener warning")
	}
}

func testFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
