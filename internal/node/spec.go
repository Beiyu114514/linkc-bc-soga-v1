// Package node normalizes panel objects into validated runtime specs.
package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// Spec is a validated panel node.
type Spec struct {
	Basic       panel.Basic
	Config      panel.Config
	NodeType    string
	TrafficRate float64
}

// LocalRuntime carries local-only settings not supplied by the panel.
type LocalRuntime struct {
	Listen           string
	CertPEM          string
	KeyPEM           string
	Certificates     []xray.TLSCertificate
	AllowPrivate     bool
	Sniffing         bool
	LimitsMode       string
	BurstBytes       int
	DeviceIPv6Prefix int
	ALPN             []string
	MinVersion       string
	ECHServerKeys    string
	Decryption       string
	RealityXVER      int
	MLDSA65Seed      string
	XHTTP            *xray.XHTTPConfig
	GRPC             *xray.GRPCConfig
	Audit            xray.AuditChecker
}

// FromPanel validates and normalizes a panel node.
func FromPanel(n panel.Node) (Spec, error) {
	s := Spec{
		Basic:       n.Basic,
		Config:      n.Config,
		NodeType:    strings.ToLower(strings.TrimSpace(n.NodeType)),
		TrafficRate: n.TrafficRate,
	}
	if s.NodeType == "" {
		s.NodeType = "vless"
	}
	if s.NodeType != "vless" {
		return Spec{}, fmt.Errorf("node_type %q is not supported (want vless)", s.NodeType)
	}
	s.Config.StreamType = strings.ToLower(strings.TrimSpace(s.Config.StreamType))
	s.Config.TLSType = strings.ToLower(strings.TrimSpace(s.Config.TLSType))
	s.Config.Flow = strings.TrimSpace(s.Config.Flow)
	if s.Config.StreamType == "" {
		s.Config.StreamType = "tcp"
	}
	if s.Config.TLSType == "" {
		s.Config.TLSType = "none"
	}
	if s.Config.TLSType == "xtls" {
		return Spec{}, fmt.Errorf("tls_type=xtls is not supported by modern Xray; use tls or reality (the target site maps xtls to none)")
	}
	if s.Config.Port < 1 || s.Config.Port > 65535 {
		return Spec{}, fmt.Errorf("port %d is out of range", s.Config.Port)
	}
	if strings.ContainsRune(s.Config.Path, 0) || strings.ContainsAny(s.Config.Path, "\r\n") {
		return Spec{}, fmt.Errorf("path contains invalid characters")
	}
	if strings.ContainsRune(s.Config.ServiceName, 0) || strings.ContainsAny(s.Config.ServiceName, "\r\n") {
		return Spec{}, fmt.Errorf("service_name contains invalid characters")
	}
	if len(s.Config.Path) > 4096 || len(s.Config.ServiceName) > 256 {
		return Spec{}, fmt.Errorf("path/service_name too long")
	}
	for _, sn := range s.Config.ServerNames {
		if sn == "" || len(sn) > 253 || strings.ContainsAny(sn, "\x00\r\n") {
			return Spec{}, fmt.Errorf("invalid server_names entry %q", sn)
		}
	}
	for _, sid := range s.Config.ShortIDs {
		if len(sid) > 16 || len(sid)%2 != 0 {
			return Spec{}, fmt.Errorf("invalid short_id %q", sid)
		}
		if _, err := hex.DecodeString(sid); err != nil {
			return Spec{}, fmt.Errorf("invalid short_id %q: %w", sid, err)
		}
	}
	if s.Config.TLSType == "reality" {
		if s.Config.PrivateKey == "" {
			return Spec{}, fmt.Errorf("reality private_key is empty")
		}
		if len(s.Config.ServerNames) == 0 {
			return Spec{}, fmt.Errorf("reality server_names is empty")
		}
		if s.Config.Dest == "" {
			return Spec{}, fmt.Errorf("reality dest is empty")
		}
		if _, _, err := net.SplitHostPort(s.Config.Dest); err != nil {
			return Spec{}, fmt.Errorf("reality dest must be host:port: %w", err)
		}
	}
	return s, nil
}

// Xray converts the panel node plus local material into an xray.NodeConfig.
func (s Spec) Xray(local LocalRuntime) (xray.NodeConfig, error) {
	cfg := xray.NodeConfig{
		Listen:           local.Listen,
		Port:             s.Config.Port,
		StreamType:       s.Config.StreamType,
		TLSType:          s.Config.TLSType,
		Flow:             s.Config.Flow,
		ServerNames:      append([]string(nil), s.Config.ServerNames...),
		PrivateKey:       s.Config.PrivateKey,
		ShortIDs:         append([]string(nil), s.Config.ShortIDs...),
		Dest:             s.Config.Dest,
		Path:             s.Config.Path,
		ServiceName:      s.Config.ServiceName,
		XVER:             firstNonZero(local.RealityXVER, s.Config.XVER),
		SpeedLimit:       s.Basic.SpeedLimit,
		AllowPrivate:     local.AllowPrivate,
		CertPEM:          local.CertPEM,
		KeyPEM:           local.KeyPEM,
		Certificates:     append([]xray.TLSCertificate(nil), local.Certificates...),
		Sniffing:         local.Sniffing,
		LimitsMode:       local.LimitsMode,
		BurstBytes:       local.BurstBytes,
		DeviceIPv6Prefix: local.DeviceIPv6Prefix,
		ALPN:             append([]string(nil), local.ALPN...),
		MinVersion:       local.MinVersion,
		Decryption:       firstNonEmpty(s.Config.Decryption, local.Decryption),
		ECHServerKeys:    firstNonEmpty(s.Config.ECHKey, local.ECHServerKeys),
		MLDSA65Seed:      local.MLDSA65Seed,
		XHTTP:            firstXHTTP(convertXHTTP(s.Config.XHTTP), local.XHTTP),
		GRPC:             firstGRPC(convertGRPC(s.Config.GRPC), local.GRPC),
		Audit:            local.Audit,
	}
	if err := xray.ValidateConfig(cfg); err != nil {
		return xray.NodeConfig{}, err
	}
	return cfg, nil
}

func convertXHTTP(in *panel.XHTTPConfig) *xray.XHTTPConfig {
	if in == nil {
		return nil
	}
	return &xray.XHTTPConfig{
		Host: in.Host, Path: in.Path, Mode: in.Mode,
		Headers: cloneMap(in.Headers),
	}
}

func convertGRPC(in *panel.GRPCConfig) *xray.GRPCConfig {
	if in == nil {
		return nil
	}
	return &xray.GRPCConfig{
		Authority: in.Authority, ServiceName: in.ServiceName, MultiMode: in.MultiMode,
		IdleTimeout: in.IdleTimeout, HealthCheckTimeout: in.HealthCheckTimeout,
		PermitWithoutStream: in.PermitWithoutStream, InitialWindowsSize: in.InitialWindowsSize,
		UserAgent: in.UserAgent,
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func firstXHTTP(a, b *xray.XHTTPConfig) *xray.XHTTPConfig {
	if a != nil {
		return a
	}
	return b
}

func firstGRPC(a, b *xray.GRPCConfig) *xray.GRPCConfig {
	if a != nil {
		return a
	}
	return b
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// InboundFingerprint hashes only fields that require an inbound swap.
// Speed limits, intervals and local freedom/sniffing settings are excluded.
func InboundFingerprint(cfg xray.NodeConfig) string {
	payload := struct {
		Listen        string
		Port          int
		Stream        string
		TLS           string
		Flow          string
		ServerNames   []string
		PrivateKey    string
		ShortIDs      []string
		Dest          string
		Path          string
		ServiceName   string
		XVER          int
		CertPEM       string
		KeyPEM        string
		Certificates  []xray.TLSCertificate
		Decryption    string
		ECHServerKeys string
		MLDSA65Seed   string
		XHTTP         *xray.XHTTPConfig
		GRPC          *xray.GRPCConfig
	}{
		Listen: cfg.Listen, Port: cfg.Port, Stream: cfg.StreamType, TLS: cfg.TLSType,
		Flow: cfg.Flow, ServerNames: cfg.ServerNames, PrivateKey: cfg.PrivateKey,
		ShortIDs: cfg.ShortIDs, Dest: cfg.Dest, Path: cfg.Path,
		ServiceName: cfg.ServiceName, XVER: cfg.XVER, CertPEM: cfg.CertPEM, KeyPEM: cfg.KeyPEM,
		Certificates: cfg.Certificates, Decryption: cfg.Decryption, ECHServerKeys: cfg.ECHServerKeys,
		MLDSA65Seed: cfg.MLDSA65Seed,
		XHTTP:       cfg.XHTTP, GRPC: cfg.GRPC,
	}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SortUsers returns users ordered by ID for deterministic output/tests.
func SortUsers(users []xray.User) {
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
}
