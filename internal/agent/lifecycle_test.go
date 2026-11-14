package agent

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
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
