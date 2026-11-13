// Package admin serves a small Unix-socket HTTP status/metrics surface.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/agent"
)

// Server is the admin Unix-socket server.
type Server struct {
	path string
	ln   net.Listener
	srv  *http.Server
}

func Start(path string, snapshot func() agent.Snapshot) (*Server, error) {
	if path == "" {
		return nil, fmt.Errorf("admin: empty socket path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("admin: listen %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		s := snapshot()
		if !s.Ready {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s := snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE linkc_ready gauge\nlinkc_ready %d\n", boolInt(s.Ready))
		fmt.Fprintf(w, "# TYPE linkc_degraded gauge\nlinkc_degraded %d\n", boolInt(s.Degraded))
		fmt.Fprintf(w, "# TYPE linkc_users gauge\nlinkc_users %d\n", s.Users)
		fmt.Fprintf(w, "# TYPE linkc_online_ips gauge\nlinkc_online_ips %d\n", s.OnlineIPs)
		fmt.Fprintf(w, "# TYPE linkc_traffic_pending_bytes gauge\nlinkc_traffic_pending_bytes %d\n", s.PendingBytes)
		fmt.Fprintf(w, "# TYPE linkc_limits_mode gauge\nlinkc_limits_mode{mode=%q} 1\n", s.LimitsMode)
		fmt.Fprintf(w, "# TYPE linkc_node_speed_mbps gauge\nlinkc_node_speed_mbps %g\n", s.NodeSpeed)
		fmt.Fprintf(w, "# TYPE linkc_active_tcp gauge\nlinkc_active_tcp %d\n", s.ActiveTCP)
		fmt.Fprintf(w, "# TYPE linkc_active_devices gauge\nlinkc_active_devices %d\n", s.ActiveDevices)
		fmt.Fprintf(w, "# TYPE linkc_limit_rejects_total counter\nlinkc_limit_rejects_total{reason=\"tcp\"} %d\nlinkc_limit_rejects_total{reason=\"device\"} %d\n", s.RejectsTCP, s.RejectsDevice)
		fmt.Fprintf(w, "# TYPE linkc_audit_rules gauge\nlinkc_audit_rules %d\n", s.AuditRules)
		fmt.Fprintf(w, "# TYPE linkc_audit_hits_total counter\nlinkc_audit_hits_total %d\n", s.AuditHits)
		fmt.Fprintf(w, "# TYPE linkc_audit_reported_total counter\nlinkc_audit_reported_total %d\n", s.AuditReported)
		fmt.Fprintf(w, "# TYPE linkc_audit_blocked_total counter\nlinkc_audit_blocked_total %d\n", s.AuditBlocked)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	s := &Server{path: path, ln: ln, srv: srv}
	go func() { _ = srv.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	_ = os.Remove(s.path)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
