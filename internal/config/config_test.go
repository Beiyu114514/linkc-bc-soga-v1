package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndEnv(t *testing.T) {
	t.Setenv("TEST_PANEL_KEY", "secret")
	path := writeConfig(t, `
panel:
  base_url: "http://127.0.0.1:8080/base/"
  api_key: "${TEST_PANEL_KEY}"
  node_id: "12"
  allow_http: true
storage:
  path: "`+filepath.Join(t.TempDir(), "state")+`"
node:
  listen: "127.0.0.1"
  pull_interval: 5s
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Panel.APIKey != "secret" {
		t.Fatalf("api key = %q", c.Panel.APIKey)
	}
	if c.Panel.BaseURL != "http://127.0.0.1:8080/base" {
		t.Fatalf("base = %q", c.Panel.BaseURL)
	}
	if c.Panel.NodeType != "vless" || c.Panel.Profile != "sspanel-uim-soga" {
		t.Fatalf("defaults = %+v", c.Panel)
	}
	if !c.Panel.TLS.Verify {
		t.Fatal("TLS verify should default true")
	}
	if c.Node.PullInterval.Duration() != 5*time.Second {
		t.Fatalf("pull = %v", c.Node.PullInterval)
	}
	if c.Node.PullInterval.Duration() != 5*time.Second || c.Node.PushInterval.Duration() != time.Minute {
		t.Fatalf("intervals = %v/%v", c.Node.PullInterval, c.Node.PushInterval)
	}
	if c.StateDBPath() != filepath.Join(c.Storage.Path, "state.db") {
		t.Fatalf("state path = %s", c.StateDBPath())
	}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsUnknownAndMissing(t *testing.T) {
	path := writeConfig(t, `
panel:
  base_url: "https://example.com"
  api_key: "k"
  node_id: "1"
unknown_field: true
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
	path = writeConfig(t, `
panel:
  base_url: "https://example.com"
  node_id: "1"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadEnvExpansionTyped(t *testing.T) {
	t.Setenv("T_MAX_ATTEMPTS", "7")
	t.Setenv("T_PANEL_KEY", "p@ss: #not-a-comment")
	path := writeConfig(t, `
panel:
  base_url: "http://127.0.0.1:8080"
  api_key: "${T_PANEL_KEY}"
  node_id: "1"
  allow_http: true
  max_attempts: ${T_MAX_ATTEMPTS}
storage:
  path: "`+t.TempDir()+`"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Panel.APIKey != "p@ss: #not-a-comment" {
		t.Fatalf("api key = %q", c.Panel.APIKey)
	}
	if c.Panel.MaxAttempts != 7 {
		t.Fatalf("max_attempts = %d", c.Panel.MaxAttempts)
	}
}

func TestCheckDoesNotCreateStorage(t *testing.T) {
	c := Default()
	c.Panel.BaseURL = "https://example.com"
	c.Panel.APIKey = "k"
	c.Panel.NodeID = "1"
	c.Storage.Path = filepath.Join(t.TempDir(), "does-not-exist", "state")
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Storage.Path); !os.IsNotExist(err) {
		t.Fatalf("Check created storage path (stat err = %v)", err)
	}
}

func TestValidateTrafficBounds(t *testing.T) {
	c := Default()
	c.Panel.BaseURL = "https://example.com"
	c.Panel.APIKey = "k"
	c.Panel.NodeID = "1"
	c.Node.Traffic.BatchBytes = 0
	if err := c.Validate(); err == nil {
		t.Fatal("expected batch_bytes error")
	}
}

func TestValidateTransportEnums(t *testing.T) {
	c := Default()
	c.Panel.BaseURL = "https://example.com"
	c.Panel.APIKey = "k"
	c.Panel.NodeID = "1"
	c.Storage.Path = t.TempDir()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Node.Status.Mode = "bad"
	if err := c.Validate(); err == nil {
		t.Fatal("expected status mode error")
	}
}
