package main

import (
	"encoding/json"
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
