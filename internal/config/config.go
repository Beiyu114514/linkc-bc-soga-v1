// Package config loads and validates the local YAML configuration.
package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML-friendly time.Duration ("15s", "1m").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	v, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Config is the complete local configuration.
type Config struct {
	Panel   PanelConfig   `yaml:"panel"`
	Node    NodeConfig    `yaml:"node"`
	Storage StorageConfig `yaml:"storage"`
	Logging LoggingConfig `yaml:"logging"`
	Admin   AdminConfig   `yaml:"admin"`
}

type PanelConfig struct {
	BaseURL         string   `yaml:"base_url"`
	Profile         string   `yaml:"profile"`
	APIKey          string   `yaml:"api_key"`
	APIKeyFile      string   `yaml:"api_key_file"`
	NodeID          string   `yaml:"node_id"`
	NodeType        string   `yaml:"node_type"`
	Timeout         Duration `yaml:"timeout"`
	AllowHTTP       bool     `yaml:"allow_http"`
	FollowRedirects bool     `yaml:"follow_redirects"`
	TLS             PanelTLS `yaml:"tls"`
	Proxy           string   `yaml:"proxy"`
	MaxResponseSize int64    `yaml:"max_response_bytes"`
	MaxAttempts     int      `yaml:"max_attempts"`
	UserAgent       string   `yaml:"user_agent"`
}

type PanelTLS struct {
	Verify         bool   `yaml:"verify"`
	CAFile         string `yaml:"ca_file"`
	ServerName     string `yaml:"server_name"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
}

type NodeConfig struct {
	Listen          string        `yaml:"listen"`
	PullInterval    Duration      `yaml:"pull_interval"`
	PushInterval    Duration      `yaml:"push_interval"`
	CollectInterval Duration      `yaml:"collect_interval"`
	StatusInterval  Duration      `yaml:"status_interval"`
	TLS             NodeTLS       `yaml:"tls"`
	VLESS           VLESSConfig   `yaml:"vless"`
	XHTTP           XHTTPConfig   `yaml:"xhttp"`
	GRPC            GRPCConfig    `yaml:"grpc"`
	Reality         RealityConfig `yaml:"reality"`
	Sniffing        Sniffing      `yaml:"sniffing"`
	Limits          LimitsConfig  `yaml:"limits"`
	Freedom         FreedomConfig `yaml:"freedom"`
	Audit           AuditConfig   `yaml:"audit"`
	Users           UsersConfig   `yaml:"users"`
	Traffic         TrafficConfig `yaml:"traffic"`
	AliveIP         AliveIPConfig `yaml:"alive_ip"`
	Status          StatusConfig  `yaml:"status"`
	Geodata         GeodataConfig `yaml:"geodata"`
}

type CertPair struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type NodeTLS struct {
	Certificates  []CertPair `yaml:"certificates"`
	ALPN          []string   `yaml:"alpn"`
	MinVersion    string     `yaml:"min_version"`
	ECHServerKeys string     `yaml:"ech_server_keys"`
}

// VLESSConfig carries optional extensions the panel does not send.
type VLESSConfig struct {
	Decryption string `yaml:"decryption"`
}

// XHTTPConfig is a local override for panels that do not send xhttp_settings.
type XHTTPConfig struct {
	Host    string            `yaml:"host"`
	Path    string            `yaml:"path"`
	Mode    string            `yaml:"mode"`
	Headers map[string]string `yaml:"headers"`
}

// GRPCConfig is a local override for panels that do not send grpc_settings.
type GRPCConfig struct {
	Authority           string `yaml:"authority"`
	ServiceName         string `yaml:"service_name"`
	MultiMode           bool   `yaml:"multi_mode"`
	IdleTimeout         int32  `yaml:"idle_timeout"`
	HealthCheckTimeout  int32  `yaml:"health_check_timeout"`
	PermitWithoutStream bool   `yaml:"permit_without_stream"`
	InitialWindowsSize  int32  `yaml:"initial_windows_size"`
	UserAgent           string `yaml:"user_agent"`
}

type RealityConfig struct {
	XVER        int    `yaml:"xver"`
	MLDSA65Seed string `yaml:"mldsa65_seed"`
}

type Sniffing struct {
	Mode string `yaml:"mode"` // auto | off
}

type LimitsConfig struct {
	Mode                  string `yaml:"mode"` // enforce | report | off
	DeviceLimitIPv6Prefix int    `yaml:"device_limit_ipv6_prefix"`
	BurstBytes            int    `yaml:"burst_bytes"`
}

type FreedomConfig struct {
	AllowPrivateTargets bool `yaml:"allow_private_targets"`
}

type AuditConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Action        string   `yaml:"action"`
	RuleMode      string   `yaml:"rule_mode"`
	WhiteListFile string   `yaml:"whitelist_file"`
	DedupeTTL     Duration `yaml:"dedupe_ttl"`
	StrictRules   bool     `yaml:"strict_rules"`
	ResolveDomain bool     `yaml:"resolve_domain_for_ip_rules"`
}

type UsersConfig struct {
	AllowEmpty      bool   `yaml:"allow_empty"`
	MassRemoveGuard string `yaml:"mass_remove_guard"` // warn | reject
	MaxUsers        int    `yaml:"max_users"`
}

type TrafficConfig struct {
	IncludeOnlineZero bool  `yaml:"include_online_zero"`
	BatchUsers        int   `yaml:"batch_users"`
	BatchBytes        int64 `yaml:"batch_bytes"`
}

type AliveIPConfig struct {
	Mode      string `yaml:"mode"` // changed | full
	FullEvery int    `yaml:"full_every"`
}

type StatusConfig struct {
	Mode string `yaml:"mode"` // load_fraction | doc
}

type GeodataConfig struct {
	Dir string `yaml:"dir"`
}

type StorageConfig struct {
	Path            string `yaml:"path"`
	MaxJournalBytes int64  `yaml:"max_journal_bytes"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

type AdminConfig struct {
	Socket  string `yaml:"socket"`
	Metrics bool   `yaml:"metrics"`
	Pprof   bool   `yaml:"pprof"`
}

// Default returns defaults that are overlaid by the YAML file.
func Default() Config {
	return Config{
		Panel: PanelConfig{
			Profile:         "sspanel-uim-soga",
			NodeType:        "vless",
			Timeout:         Duration(15 * time.Second),
			TLS:             PanelTLS{Verify: true},
			MaxResponseSize: 32 << 20,
			MaxAttempts:     3,
			UserAgent:       "linkc-node/" + "dev",
		},
		Node: NodeConfig{
			Listen:          "0.0.0.0",
			PullInterval:    Duration(60 * time.Second),
			PushInterval:    Duration(60 * time.Second),
			CollectInterval: Duration(30 * time.Second),
			StatusInterval:  Duration(5 * time.Second),
			Sniffing:        Sniffing{Mode: "auto"},
			Limits:          LimitsConfig{Mode: "enforce", DeviceLimitIPv6Prefix: 128, BurstBytes: 65536},
			AliveIP:         AliveIPConfig{Mode: "changed", FullEvery: 20},
			Users:           UsersConfig{AllowEmpty: true, MassRemoveGuard: "warn", MaxUsers: 100000},
			Traffic:         TrafficConfig{BatchUsers: 500, BatchBytes: 1 << 20},
			Status:          StatusConfig{Mode: "load_fraction"},
			Geodata:         GeodataConfig{Dir: "/usr/share/xray"},
			Audit:           AuditConfig{Enabled: true, Action: "block", RuleMode: "auto", DedupeTTL: Duration(24 * time.Hour)},
		},
		Storage: StorageConfig{Path: "/var/lib/linkc-node", MaxJournalBytes: 64 << 20},
		Logging: LoggingConfig{Level: "info", Format: "json"},
		Admin:   AdminConfig{Socket: "/run/linkc-node/admin.sock", Metrics: true},
	}
}

// Load reads, expands and validates a YAML config file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := expandNode(&root); err != nil {
		return Config{}, err
	}
	normalized, err := yaml.Marshal(&root)
	if err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	c := Default()
	dec := yaml.NewDecoder(bytes.NewReader(normalized))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Normalize(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Normalize applies derived values and basic syntax validation.
func (c *Config) Normalize() error {
	c.Panel.BaseURL = strings.TrimRight(strings.TrimSpace(c.Panel.BaseURL), "/")
	c.Panel.NodeID = strings.TrimSpace(c.Panel.NodeID)
	c.Panel.NodeType = strings.ToLower(strings.TrimSpace(c.Panel.NodeType))
	c.Node.Listen = strings.TrimSpace(c.Node.Listen)
	c.Node.Sniffing.Mode = strings.ToLower(strings.TrimSpace(c.Node.Sniffing.Mode))
	c.Node.Limits.Mode = strings.ToLower(strings.TrimSpace(c.Node.Limits.Mode))
	c.Node.Users.MassRemoveGuard = strings.ToLower(strings.TrimSpace(c.Node.Users.MassRemoveGuard))
	c.Node.AliveIP.Mode = strings.ToLower(strings.TrimSpace(c.Node.AliveIP.Mode))
	c.Node.Audit.Action = strings.ToLower(strings.TrimSpace(c.Node.Audit.Action))
	c.Node.Audit.RuleMode = strings.ToLower(strings.TrimSpace(c.Node.Audit.RuleMode))
	c.Node.Status.Mode = strings.ToLower(strings.TrimSpace(c.Node.Status.Mode))
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))

	if c.Panel.Profile == "" {
		c.Panel.Profile = "sspanel-uim-soga"
	}
	if c.Panel.NodeType == "" {
		c.Panel.NodeType = "vless"
	}
	if c.Panel.Timeout <= 0 {
		c.Panel.Timeout = Duration(15 * time.Second)
	}
	if c.Panel.MaxResponseSize <= 0 {
		c.Panel.MaxResponseSize = 32 << 20
	}
	if c.Panel.MaxAttempts <= 0 {
		c.Panel.MaxAttempts = 3
	}
	if c.Node.Listen == "" {
		c.Node.Listen = "0.0.0.0"
	} else if strings.EqualFold(strings.TrimSpace(c.Node.Listen), "localhost") {
		c.Node.Listen = "127.0.0.1"
	}
	if c.Node.PullInterval <= 0 {
		c.Node.PullInterval = Duration(60 * time.Second)
	}
	if c.Node.PushInterval <= 0 {
		c.Node.PushInterval = Duration(60 * time.Second)
	}
	if c.Node.CollectInterval <= 0 {
		c.Node.CollectInterval = Duration(30 * time.Second)
	}
	if c.Node.StatusInterval <= 0 {
		c.Node.StatusInterval = Duration(5 * time.Second)
	}
	if c.Node.Users.MaxUsers <= 0 {
		c.Node.Users.MaxUsers = 100000
	}
	if c.Node.Traffic.BatchUsers <= 0 {
		c.Node.Traffic.BatchUsers = 500
	}
	if c.Node.Traffic.BatchBytes <= 0 {
		c.Node.Traffic.BatchBytes = 1 << 20
	}
	if c.Storage.Path == "" {
		c.Storage.Path = "/var/lib/linkc-node"
	}
	if c.Storage.MaxJournalBytes <= 0 {
		c.Storage.MaxJournalBytes = 64 << 20
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Admin.Socket == "" {
		c.Admin.Socket = "/run/linkc-node/admin.sock"
	}

	// API key may come from a file; environment substitution already happened.
	if c.Panel.APIKey == "" && c.Panel.APIKeyFile != "" {
		b, err := os.ReadFile(c.Panel.APIKeyFile)
		if err != nil {
			return fmt.Errorf("read api_key_file: %w", err)
		}
		c.Panel.APIKey = strings.TrimSpace(string(b))
	}
	return c.Validate()
}

func (c Config) Validate() error {
	if c.Panel.BaseURL == "" {
		return fmt.Errorf("panel.base_url is required")
	}
	u, err := url.Parse(c.Panel.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("panel.base_url is invalid: %q", c.Panel.BaseURL)
	}
	if u.Scheme != "https" && !c.Panel.AllowHTTP {
		return fmt.Errorf("panel.base_url must use https (set panel.allow_http for tests)")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("panel.base_url scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("panel.base_url must not contain userinfo")
	}
	if c.Panel.Profile != "sspanel-uim-soga" {
		return fmt.Errorf("unsupported panel.profile %q", c.Panel.Profile)
	}
	if c.Panel.APIKey == "" {
		return fmt.Errorf("panel.api_key is required")
	}
	if c.Panel.NodeID == "" {
		return fmt.Errorf("panel.node_id is required")
	}
	if c.Panel.NodeType != "vless" {
		return fmt.Errorf("panel.node_type must be vless (got %q)", c.Panel.NodeType)
	}
	if c.Panel.FollowRedirects {
		return fmt.Errorf("panel.follow_redirects is not supported (avoids leaking API-KEY)")
	}
	if ip := net.ParseIP(c.Node.Listen); ip == nil {
		return fmt.Errorf("node.listen must be an IP address, got %q", c.Node.Listen)
	}
	if c.Node.Limits.Mode != "enforce" && c.Node.Limits.Mode != "report" && c.Node.Limits.Mode != "off" {
		return fmt.Errorf("node.limits.mode must be enforce|report|off")
	}
	if c.Node.Sniffing.Mode != "auto" && c.Node.Sniffing.Mode != "off" {
		return fmt.Errorf("node.sniffing.mode must be auto|off")
	}
	if c.Node.Users.MassRemoveGuard != "warn" && c.Node.Users.MassRemoveGuard != "reject" {
		return fmt.Errorf("node.users.mass_remove_guard must be warn|reject")
	}
	if c.Node.Status.Mode != "load_fraction" && c.Node.Status.Mode != "doc" {
		return fmt.Errorf("node.status.mode must be load_fraction|doc")
	}
	if c.Node.AliveIP.Mode != "changed" && c.Node.AliveIP.Mode != "full" {
		return fmt.Errorf("node.alive_ip.mode must be changed|full")
	}
	if c.Node.AliveIP.FullEvery < 0 {
		return fmt.Errorf("node.alive_ip.full_every must be >= 0")
	}
	if c.Node.Audit.Action != "block" && c.Node.Audit.Action != "report" {
		return fmt.Errorf("node.audit.action must be block|report")
	}
	if c.Node.TLS.ECHServerKeys != "" {
		if _, err := base64.StdEncoding.DecodeString(c.Node.TLS.ECHServerKeys); err != nil {
			return fmt.Errorf("node.tls.ech_server_keys must be base64: %w", err)
		}
	}
	if d := c.Node.VLESS.Decryption; d != "" && (len(d) > 8192 || strings.ContainsAny(d, "\x00\r\n \t")) {
		return fmt.Errorf("node.vless.decryption contains invalid characters")
	}
	if m := c.Node.XHTTP.Mode; m != "" && m != "auto" && m != "packet-up" && m != "stream-up" && m != "stream-one" {
		return fmt.Errorf("node.xhttp.mode must be auto|packet-up|stream-up|stream-one")
	}
	for k := range c.Node.XHTTP.Headers {
		if strings.EqualFold(k, "host") {
			return fmt.Errorf("node.xhttp.headers must not contain Host")
		}
	}
	if c.Node.GRPC.IdleTimeout < 0 || c.Node.GRPC.HealthCheckTimeout < 0 || c.Node.GRPC.InitialWindowsSize < 0 {
		return fmt.Errorf("node.grpc timeouts/window must be >= 0")
	}
	if m := c.Node.Audit.RuleMode; m != "auto" && m != "regex" && m != "keyword" && m != "prefixed" {
		return fmt.Errorf("node.audit.rule_mode must be auto|regex|keyword|prefixed")
	}
	if c.Node.Traffic.BatchUsers > 500 {
		return fmt.Errorf("node.traffic.batch_users must be <= 500")
	}
	if c.Node.Traffic.BatchBytes <= 0 || c.Node.Traffic.BatchBytes > 1<<30 {
		return fmt.Errorf("node.traffic.batch_bytes must be in (0, 1GiB]")
	}
	if c.Storage.MaxJournalBytes <= 0 {
		return fmt.Errorf("storage.max_journal_bytes must be > 0")
	}
	if c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return fmt.Errorf("logging.level must be debug|info|warn|error")
	}
	if c.Logging.Format != "json" && c.Logging.Format != "text" {
		return fmt.Errorf("logging.format must be json|text")
	}
	return nil
}

// Check performs side-effect checks that require the filesystem/CA material.
func (c Config) Check() error {
	if err := c.Validate(); err != nil {
		return err
	}
	for _, p := range []string{c.Panel.TLS.CAFile, c.Panel.TLS.ClientCertFile, c.Panel.TLS.ClientKeyFile} {
		if p != "" {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("panel TLS file %s: %w", p, err)
			}
		}
	}
	if c.Panel.TLS.ClientCertFile != "" || c.Panel.TLS.ClientKeyFile != "" {
		if _, err := tls.LoadX509KeyPair(c.Panel.TLS.ClientCertFile, c.Panel.TLS.ClientKeyFile); err != nil {
			return fmt.Errorf("panel client certificate: %w", err)
		}
	}
	if c.Panel.TLS.CAFile != "" {
		b, err := os.ReadFile(c.Panel.TLS.CAFile)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return fmt.Errorf("panel.tls.ca_file contains no certificates")
		}
	}
	for _, cp := range c.Node.TLS.Certificates {
		if cp.CertFile == "" || cp.KeyFile == "" {
			return fmt.Errorf("node.tls.certificates requires cert_file and key_file")
		}
		if _, err := tls.LoadX509KeyPair(cp.CertFile, cp.KeyFile); err != nil {
			return fmt.Errorf("node TLS certificate %s: %w", cp.CertFile, err)
		}
	}
	if c.Node.Audit.WhiteListFile != "" {
		if _, err := os.Stat(c.Node.Audit.WhiteListFile); err != nil {
			return fmt.Errorf("node.audit.whitelist_file: %w", err)
		}
	}
	return nil
}

// StateDBPath returns the bbolt path.
func (c Config) StateDBPath() string { return filepath.Join(c.Storage.Path, "state.db") }

func expandNode(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		if strings.Contains(n.Value, "${") {
			v, err := expandString(n.Value)
			if err != nil {
				return err
			}
			setScalar(n, v)
		}
		return nil
	}
	for _, c := range n.Content {
		if err := expandNode(c); err != nil {
			return err
		}
	}
	return nil
}

// setScalar writes an expanded value while preserving YAML typing: quoted
// scalars stay strings; unquoted placeholders may become ints/bools; anything
// ambiguous is forced to a safely quoted string.
func setScalar(n *yaml.Node, v string) {
	if n.Style == yaml.DoubleQuotedStyle || n.Style == yaml.SingleQuotedStyle ||
		n.Style == yaml.LiteralStyle || n.Style == yaml.FoldedStyle {
		n.Value = v
		n.Tag = "!!str"
		return
	}
	var tmp yaml.Node
	if err := yaml.Unmarshal([]byte(v), &tmp); err == nil && len(tmp.Content) == 1 && tmp.Content[0].Kind == yaml.ScalarNode {
		n.Tag, n.Style = tmp.Content[0].Tag, tmp.Content[0].Style
	} else {
		n.Tag, n.Style = "!!str", yaml.DoubleQuotedStyle
	}
	n.Value = v
}

// expandString replaces ${NAME} placeholders in a single scalar.
func expandString(s string) (string, error) {
	var out strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			out.WriteString(s)
			break
		}
		out.WriteString(s[:i])
		j := strings.Index(s[i+2:], "}")
		if j < 0 {
			return "", fmt.Errorf("unterminated environment variable in config")
		}
		name := s[i+2 : i+2+j]
		val, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		out.WriteString(val)
		s = s[i+2+j+1:]
	}
	return out.String(), nil
}
