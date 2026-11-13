package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/agent"
)

func TestUnixEndpoints(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	s, err := Start(sock, func() agent.Snapshot {
		return agent.Snapshot{Ready: true, Users: 3, PendingBytes: 42}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	c := &http.Client{Transport: tr}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := c.Get("http://unix" + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
	}
	resp, err := c.Get("http://unix/status")
	if err != nil {
		t.Fatal(err)
	}
	var snap agent.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if snap.Users != 3 || snap.PendingBytes != 42 {
		t.Fatalf("snap = %+v", snap)
	}
}
