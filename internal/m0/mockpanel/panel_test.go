package mockpanel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testPanel() *Panel {
	p := New(Config{
		APIKey:       "test-key",
		NodeID:       7,
		NodeType:     "vless",
		PullInterval: 60,
		PushInterval: 30,
		SpeedLimit:   100,
		TrafficRate:  2,
		Port:         24443,
		StreamType:   "tcp",
		TLSType:      "reality",
		Flow:         "xtls-rprx-vision",
		ServerNames:  []string{"www.example.com"},
		PrivateKey:   "priv",
		ShortIDs:     []string{"0123456789abcdef"},
		Dest:         "www.example.com:443",
		NodeIP:       "127.0.0.1",
	})
	p.SetUsers(User{
		ID: 1, UUID: "701534e1-8540-49f3-87af-71680a1b97ff",
		SpeedLimit: 10, DeviceLimit: 2, TransferEnable: 1000, Enable: true,
	})
	p.SetRules(
		Rule{ID: 1, Name: "plain", Text: "google", Regex: ""},
		Rule{ID: 2, Name: "regex", Text: "desc", Regex: `.*google\.com`},
	)
	p.SetWhiteList("domain:google.com")
	return p
}

func request(t *testing.T, c *http.Client, method, url string, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("API-KEY", "test-key")
	req.Header.Set("NODE-ID", "7")
	req.Header.Set("NODE-TYPE", "vless")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestETagAndNodePayload(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	resp := request(t, c, http.MethodGet, srv.URL+"/api/v1/node", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	var payload map[string]any
	decode(t, resp, &payload)
	if payload["node_type"] != "vless" {
		t.Fatalf("node_type = %v", payload["node_type"])
	}
	if payload["traffic_rate"].(float64) != 2 {
		t.Fatalf("traffic_rate = %v", payload["traffic_rate"])
	}
	cfg := payload["config"].(map[string]any)
	if cfg["stream_type"] != "tcp" || cfg["tls_type"] != "reality" {
		t.Fatalf("config = %+v", cfg)
	}
	if _, ok := cfg["ech_key"]; ok {
		t.Fatal("site does not send ech_key")
	}

	resp2 := request(t, c, http.MethodGet, srv.URL+"/api/v1/node", "", map[string]string{"IF-NONE-MATCH": etag})
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestUsersQuotaAndKeepConnect(t *testing.T) {
	p := testPanel()
	u := p.users[1]
	u.U = 600
	u.D = 400 // exactly transfer_enable, over quota
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	resp := request(t, c, http.MethodGet, srv.URL+"/api/v1/users", "", nil)
	var users []map[string]any
	decode(t, resp, &users)
	if len(users) != 0 {
		t.Fatalf("expected empty users, got %+v", users)
	}

	p.SetKeepConnect(true)
	resp = request(t, c, http.MethodGet, srv.URL+"/api/v1/users", "", nil)
	decode(t, resp, &users)
	if len(users) != 1 {
		t.Fatalf("expected keep_connect user, got %+v", users)
	}
	if users[0]["speed_limit"].(float64) != 1 {
		t.Fatalf("keep_connect speed = %v", users[0]["speed_limit"])
	}

	// Node bandwidth limit is a separate empty-users path.
	p.SetNodeBandwidthLimit(100)
	p.SetNodeBandwidth(101)
	resp = request(t, c, http.MethodGet, srv.URL+"/api/v1/users", "", nil)
	decode(t, resp, &users)
	if len(users) != 0 {
		t.Fatalf("expected empty users on node bandwidth, got %+v", users)
	}
}

func TestAuditRulesAreRawAndWhiteList(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	resp := request(t, c, http.MethodGet, srv.URL+"/api/v1/audit_rules", "", nil)
	var rules []map[string]any
	decode(t, resp, &rules)
	if len(rules) != 2 {
		t.Fatalf("rules = %+v", rules)
	}
	if rules[0]["rule"] != "google" {
		t.Fatalf("plain rule = %v", rules[0]["rule"])
	}
	if rules[1]["rule"] != `.*google\.com` {
		t.Fatalf("regex rule = %v", rules[1]["rule"])
	}

	resp = request(t, c, http.MethodGet, srv.URL+"/api/v1/white_list", "", nil)
	var white []string
	decode(t, resp, &white)
	if len(white) != 1 || white[0] != "domain:google.com" {
		t.Fatalf("white = %+v", white)
	}
}

func TestTrafficRateOnlineAndAliveIP(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	body := `[{"id":1,"u":100,"d":200}]`
	resp := request(t, c, http.MethodPost, srv.URL+"/api/v1/traffic", body, nil)
	var result map[string]any
	decode(t, resp, &result)
	if result["code"].(float64) != 0 {
		t.Fatalf("traffic code = %v", result)
	}
	snap := p.Snapshot()
	if snap.Users[1].U != 200 || snap.Users[1].D != 400 { // rate = 2
		t.Fatalf("user traffic = %+v", snap.Users[1])
	}
	if snap.Config.NodeBandwidth != 300 { // raw
		t.Fatalf("node bandwidth = %d", snap.Config.NodeBandwidth)
	}
	if len(snap.Online) != 1 || snap.Online[0].OnlineUser != 1 {
		t.Fatalf("online = %+v", snap.Online)
	}

	body = `[{"id":1,"ips":["1.2.3.4","::ffff:5.6.7.8"]}]`
	resp = request(t, c, http.MethodPost, srv.URL+"/api/v1/alive_ip", body, nil)
	decode(t, resp, &result)
	snap = p.Snapshot()
	if len(snap.Alive) != 2 {
		t.Fatalf("alive = %+v", snap.Alive)
	}
	if snap.Alive[1].IP != "5.6.7.8" {
		t.Fatalf("alive ip = %+v", snap.Alive[1])
	}

	// Re-send within 60s: upsert, no new row.
	resp = request(t, c, http.MethodPost, srv.URL+"/api/v1/alive_ip", body, nil)
	decode(t, resp, &result)
	if got := len(p.Snapshot().Alive); got != 2 {
		t.Fatalf("alive rows = %d, want 2", got)
	}

	// Send a different set; old IPs are NOT cleared.
	body = `[{"id":1,"ips":["9.9.9.9"]}]`
	resp = request(t, c, http.MethodPost, srv.URL+"/api/v1/alive_ip", body, nil)
	decode(t, resp, &result)
	if got := len(p.Snapshot().Alive); got != 3 {
		t.Fatalf("alive rows = %d, want 3 (no clearing)", got)
	}
}

func TestAuditLogDailyDedupe(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	body := `[{"user_id":1,"audit_id":2},{"user_id":1,"audit_id":2}]`
	for i := 0; i < 2; i++ {
		resp := request(t, c, http.MethodPost, srv.URL+"/api/v1/audit_log", body, nil)
		var result map[string]any
		decode(t, resp, &result)
		if result["code"].(float64) != 0 {
			t.Fatalf("audit code = %v", result)
		}
	}
	if got := len(p.Snapshot().Detects); got != 1 {
		t.Fatalf("detect rows = %d, want 1", got)
	}
}

func TestStatusLoadCompatibility(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	body := `{"load":"0.235","cpu":23.5,"mem":{"total":100,"used":50},"swap":{"total":0,"used":0},"disk":{"total":100,"used":50},"uptime":99}`
	resp := request(t, c, http.MethodPost, srv.URL+"/api/v1/status", body, nil)
	var result map[string]any
	decode(t, resp, &result)
	if result["code"].(float64) != 0 {
		t.Fatalf("status code = %v", result)
	}
	snap := p.Snapshot()
	if len(snap.InfoLogs) != 1 {
		t.Fatalf("info logs = %+v", snap.InfoLogs)
	}
	if snap.InfoLogs[0].Load != "0.235" {
		t.Fatalf("load = %q", snap.InfoLogs[0].Load)
	}
	if snap.InfoLogs[0].Uptime != 99 {
		t.Fatalf("uptime = %v", snap.InfoLogs[0].Uptime)
	}
	// Site UI formula.
	if got := 0.235 * 100; got != 23.5 {
		t.Fatalf("sanity = %v", got)
	}
}

func TestAuthFailures(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/node", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	p.SetWebAPIDisabled(true)
	resp = request(t, c, http.MethodGet, srv.URL+"/api/v1/node", "", nil)
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	p.SetWebAPIDisabled(false)
	// Missing node.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/v1/node", nil)
	req.Header.Set("API-KEY", "test-key")
	req.Header.Set("NODE-ID", "999")
	resp, _ = c.Do(req)
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHeartbeatTouched(t *testing.T) {
	p := testPanel()
	srv := httptest.NewServer(p)
	defer srv.Close()
	_ = request(t, srv.Client(), http.MethodGet, srv.URL+"/api/v1/node", "", nil).Body.Close()
	time.Sleep(10 * time.Millisecond)
	if p.Snapshot().Config.NodeHeartbeat == 0 {
		t.Fatal("heartbeat not updated")
	}
}
