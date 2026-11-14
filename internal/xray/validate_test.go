package xray_test

import (
	"testing"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// TestValidateRealityShortIDs guards #8: the check must match Xray, which
// rejects an empty shortIds list but accepts an empty string (it hex-decodes
// to an all-zero id and is the usual way to disable shortIds).
func TestValidateRealityShortIDs(t *testing.T) {
	n := baseNode(443)
	n.TLSType = "reality"
	n.PrivateKey = "priv"
	n.ServerNames = []string{"example.com"}
	n.Dest = "example.com:443"

	n.ShortIDs = nil
	if err := xray.ValidateConfig(n); err == nil {
		t.Fatal("reality with no short_ids must be rejected")
	}
	n.ShortIDs = []string{}
	if err := xray.ValidateConfig(n); err == nil {
		t.Fatal("reality with an empty short_ids list must be rejected")
	}
	n.ShortIDs = []string{""}
	if err := xray.ValidateConfig(n); err != nil {
		t.Fatalf("empty short_id must be accepted: %v", err)
	}
	n.ShortIDs = []string{"0123456789abcdef"}
	if err := xray.ValidateConfig(n); err != nil {
		t.Fatalf("valid short_id rejected: %v", err)
	}

	bad := map[string]string{
		"too long":  "0123456789abcdef00",
		"non-hex":   "zz",
		"odd bytes": "abc",
	}
	for name, sid := range bad {
		n.ShortIDs = []string{sid}
		if err := xray.ValidateConfig(n); err == nil {
			t.Fatalf("%s short_id %q must be rejected", name, sid)
		}
	}
}

// TestShortIDsIgnoredOutsideReality makes sure the new reality-only rule does
// not reject short_ids on plain TLS nodes.
func TestShortIDsIgnoredOutsideReality(t *testing.T) {
	n := baseNode(443)
	n.ShortIDs = []string{"0123456789abcdef"}
	if err := xray.ValidateConfig(n); err != nil {
		t.Fatalf("short_ids on a non-reality node must not fail validation: %v", err)
	}
}
