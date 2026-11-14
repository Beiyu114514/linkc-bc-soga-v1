package agent_test

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	m0engine "github.com/Beiyu114514/linkc-bc-soga-v1/internal/m0/engine"
)

// TestSoak runs a randomized fault-injection soak against a live agent. It is
// disabled by default; enable it with a duration:
//
//	SOAK_DURATION=24h go test ./internal/agent -run TestSoak -count=1 -timeout 25h
//	SOAK_DURATION=90s SOAK_SEED=7 go test ./internal/agent -run TestSoak -count=1
//
// The soak injects panel 5xx/429, webapi outages, node config (port) swaps,
// user churn, certificate rotation and traffic bursts, and asserts after every
// round that the data plane recovers, the agent stays ready and the traffic
// journal stays bounded.
func TestSoak(t *testing.T) {
	durEnv := os.Getenv("SOAK_DURATION")
	if durEnv == "" {
		t.Skip("set SOAK_DURATION (e.g. 90s or 24h) to run the soak")
	}
	dur, err := time.ParseDuration(durEnv)
	if err != nil {
		t.Fatalf("SOAK_DURATION: %v", err)
	}
	seed := uint64(1)
	if v := os.Getenv("SOAK_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("SOAK_SEED: %v", err)
		}
		seed = n
	}
	rng := rand.New(rand.NewPCG(seed, seed>>32))

	portA, portB := freePort(t), freePort(t)
	active := portA
	p, srv := startPanel(t, active)
	defer srv.Close()

	certDir := t.TempDir()
	certFile, keyFile, pin := writeCertFiles(t, certDir, "localhost")

	cfg := testConfig(t, srv.URL, t.TempDir())
	cfg.Node.TLS.Certificates = []config.CertPair{{CertFile: certFile, KeyFile: keyFile}}
	nc := p.Snapshot().Config
	nc.TLSType = "tls"
	p.SetNodeConfig(nc)

	a, stop, done := startAgent(t, cfg)
	defer stop()

	ctx := context.Background()
	target := bigTarget(t, 256<<10)
	hc := tlsSoakClient(t, ctx, active, pin)

	// get retries until the data plane answers again.
	get := func(what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := hc.Get(target.URL)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: data plane did not recover: %v", what, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	get("initial")

	deadline := time.Now().Add(dur)
	rounds, faults := 0, 0
	started := time.Now()
	for time.Now().Before(deadline) {
		rounds++
		action := "burst"
		switch rng.IntN(7) {
		case 0: // panel 5xx on a random GET endpoint
			path := []string{"/api/v1/node", "/api/v1/users", "/api/v1/audit_rules", "/api/v1/white_list"}[rng.IntN(4)]
			p.SetFault(path, 500, 3+rng.IntN(5))
			action = "panel 500 on " + path
			faults++
		case 1: // panel 429 on a random POST endpoint
			path := []string{"/api/v1/traffic", "/api/v1/alive_ip", "/api/v1/audit_log", "/api/v1/status"}[rng.IntN(4)]
			p.SetFault(path, 429, 3+rng.IntN(5))
			action = "panel 429 on " + path
			faults++
		case 2: // full webapi outage
			p.SetWebAPIDisabled(true)
			action = "webapi disabled"
			faults++
		case 3: // node config swap: move the inbound to the other port
			if active == portA {
				active = portB
			} else {
				active = portA
			}
			nc := p.Snapshot().Config
			nc.Port = active
			p.SetNodeConfig(nc)
			hc = tlsSoakClient(t, ctx, active, pin)
			action = fmt.Sprintf("port swap to %d", active)
			faults++
		case 4: // user churn within the round: drop everyone, then re-add
			p.SetUsers()
			time.Sleep(time.Duration(50+rng.IntN(200)) * time.Millisecond)
			p.SetUsers(panelUser())
			action = "user churn"
			faults++
		case 5: // certificate rotation (SIGHUP path)
			name := "localhost"
			if rng.IntN(2) == 0 {
				name = "soak.test"
			}
			certFile, keyFile, pin = writeCertFiles(t, certDir, name)
			if err := a.ReloadCerts(); err != nil {
				t.Fatalf("round %d: ReloadCerts: %v", rounds, err)
			}
			hc = tlsSoakClient(t, ctx, active, pin)
			action = "cert rotation"
			faults++
		case 6: // traffic burst
			for i := 0; i < 5; i++ {
				resp, err := hc.Get(target.URL)
				if err != nil {
					break
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}

		// Invariants after every round.
		p.SetWebAPIDisabled(false)
		get(fmt.Sprintf("round %d (%s)", rounds, action))
		if s := a.Snapshot(); !s.Ready {
			t.Fatalf("round %d (%s): agent not ready: %+v", rounds, action, s)
		}
		if s := a.Snapshot(); s.PendingBytes > 256<<20 {
			t.Fatalf("round %d (%s): journal backlog too large: %d bytes", rounds, action, s.PendingBytes)
		}
		select {
		case err := <-done:
			t.Fatalf("round %d (%s): agent exited: %v", rounds, action, err)
		default:
		}
		if rounds%20 == 0 {
			s := a.Snapshot()
			t.Logf("round %d (%v elapsed): users=%d port=%d pending=%d degraded=%v",
				rounds, time.Since(started).Round(time.Second), s.Users, s.Port, s.PendingBytes, s.Degraded)
		}
	}
	t.Logf("soak finished: %d rounds, %d fault injections in %v", rounds, faults, time.Since(started).Round(time.Second))
}

// tlsSoakClient builds a TLS VLESS client pinned to the current certificate
// (the certificate is rotated during the soak, so the pin is refreshed with
// every rotation).
func tlsSoakClient(t *testing.T, ctx context.Context, port int, pin string) *http.Client {
	t.Helper()
	socksPort := freePort(t)
	inst, err := m0engine.StartClient(ctx, m0engine.ClientConfig{
		SocksPort: socksPort, Server: "127.0.0.1", Port: port, UUID: testUUID,
		TLS: true, ServerName: "localhost", PinnedPeerCertSha256: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return socksHTTPClient(t, socksPort)
}
