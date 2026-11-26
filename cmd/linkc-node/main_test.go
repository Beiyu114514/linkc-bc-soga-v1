package main

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedactSecrets guards the dump-config output: every value that grants
// access or impersonates the node must be masked.
func TestRedactSecrets(t *testing.T) {
	raw := `{
		"inbounds": [{
			"settings": {"clients": [{"id": "uuid-secret", "email": "u1"}]},
			"streamSettings": {
				"tlsSettings": {
					"certificates": [{"certificate": ["CERT-PEM"], "key": ["KEY-PEM"]}],
					"echServerKeys": "ECH-SERVER-KEYS"
				},
				"realitySettings": {
					"privateKey": "REALITY-PRIVATE",
					"shortIds": ["0123abcd"],
					"mldsa65Seed": "MLDSA65-SEED",
					"headers": {"x": "y"}
				}
			}
		}]
	}`
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	redact(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"uuid-secret", "CERT-PEM", "KEY-PEM", "ECH-SERVER-KEYS",
		"REALITY-PRIVATE", "0123abcd", "MLDSA65-SEED",
	} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("redacted output still contains %q: %s", secret, out)
		}
	}
}

// TestRedactSnakeCase covers panels/handlers that use snake_case keys.
func TestRedactSnakeCase(t *testing.T) {
	v := map[string]any{
		"ech_server_keys": "ECH",
		"mldsa65_seed":    "SEED",
		"private_key":     "PK",
		"short_ids":       []any{"abcd"},
	}
	redact(v)
	for k, val := range v {
		if s, ok := val.(string); ok && s != "***" {
			t.Fatalf("%s not redacted: %v", k, val)
		}
		if _, ok := val.([]any); ok {
			t.Fatalf("%s not redacted: %v", k, val)
		}
	}
}

func TestRedactVLESSDecryption(t *testing.T) {
	v := map[string]any{
		"inbounds": []any{
			map[string]any{
				"settings": map[string]any{
					"decryption": "mlkem768x25519plus.native.0rtt.SECRETKEY",
				},
			},
			map[string]any{
				"settings": map[string]any{
					"decryption": "none",
				},
			},
		},
	}
	redact(v)
	inbounds := v["inbounds"].([]any)
	s0 := inbounds[0].(map[string]any)["settings"].(map[string]any)["decryption"]
	s1 := inbounds[1].(map[string]any)["settings"].(map[string]any)["decryption"]
	if s0 != "***" {
		t.Fatalf("non-none decryption must be redacted, got %v", s0)
	}
	if s1 != "none" {
		t.Fatalf("decryption=none should stay visible, got %v", s1)
	}
}

func TestQueryAdminUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ready":true,"users":3}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	body, err := queryAdmin(sock, "/status")
	if err != nil {
		t.Fatalf("queryAdmin(/status) failed: %v", err)
	}
	if !strings.Contains(string(body), `"users":3`) {
		t.Fatalf("unexpected /status body: %s", body)
	}
	if _, err := queryAdmin(sock, "/readyz"); err == nil {
		t.Fatal("expected error when /readyz returns 503")
	}
}
