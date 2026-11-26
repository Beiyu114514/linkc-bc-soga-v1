package xray

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"

	xnet "github.com/xtls/xray-core/common/net"
)

// ValidateConfig checks the transport/security matrix before touching Xray.
func ValidateConfig(node NodeConfig) error {
	streamType := strings.ToLower(strings.TrimSpace(node.StreamType))
	if streamType == "" {
		streamType = "tcp"
	}
	tlsType := strings.ToLower(strings.TrimSpace(node.TLSType))
	if tlsType == "" {
		tlsType = "none"
	}
	listen := node.Listen
	if listen == "" {
		listen = "0.0.0.0"
	}
	if net.ParseIP(listen) == nil {
		return fmt.Errorf("listen address %q is not an IP", node.Listen)
	}
	if node.Port < 1 || node.Port > 65535 {
		return fmt.Errorf("port %d is out of range", node.Port)
	}

	switch streamType {
	case "tcp", "raw":
		streamType = "raw"
	case "ws", "grpc", "xhttp":
	case "http", "h2":
		return fmt.Errorf("transport %q is removed in modern Xray; use xhttp", node.StreamType)
	default:
		return fmt.Errorf("unsupported stream_type %q", node.StreamType)
	}
	switch tlsType {
	case "none", "tls", "reality":
	case "xtls":
		return fmt.Errorf("tls_type=xtls is not supported; use tls or reality")
	default:
		return fmt.Errorf("unsupported tls_type %q", node.TLSType)
	}

	flow := strings.TrimSpace(node.Flow)
	if flow != "" && flow != "xtls-rprx-vision" {
		return fmt.Errorf("unsupported flow %q", flow)
	}
	if flow != "" && streamType != "raw" {
		return fmt.Errorf("flow %q requires raw/tcp transport", flow)
	}
	if flow != "" && tlsType == "none" {
		return fmt.Errorf("flow %q requires tls or reality", flow)
	}
	if tlsType == "reality" && streamType == "ws" {
		return fmt.Errorf("reality is not supported with ws")
	}
	if node.XVER < 0 || node.XVER > 2 {
		return fmt.Errorf("xver must be 0, 1 or 2")
	}
	if node.SpeedLimit < 0 {
		return fmt.Errorf("speed_limit must be >= 0")
	}
	if node.BurstBytes < 0 {
		return fmt.Errorf("burst_bytes must be >= 0")
	}
	if node.DeviceIPv6Prefix < 0 || node.DeviceIPv6Prefix > 128 {
		return fmt.Errorf("device_ipv6_prefix must be 0..128")
	}
	switch strings.ToLower(node.LimitsMode) {
	case "", "enforce", "report", "off":
	default:
		return fmt.Errorf("limits mode must be enforce|report|off")
	}

	for i, c := range node.Certificates {
		if c.CertPEM == "" || c.KeyPEM == "" {
			return fmt.Errorf("certificate %d: cert/key PEM must be provided together", i)
		}
		if _, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM)); err != nil {
			return fmt.Errorf("invalid certificate %d: %w", i, err)
		}
	}
	if len(node.CertPEM) > 0 || len(node.KeyPEM) > 0 {
		if len(node.CertPEM) == 0 || len(node.KeyPEM) == 0 {
			return fmt.Errorf("certificate/key PEM must be provided together")
		}
		if _, err := tls.X509KeyPair([]byte(node.CertPEM), []byte(node.KeyPEM)); err != nil {
			return fmt.Errorf("invalid inline certificate/key: %w", err)
		}
	}
	if node.CertFile != "" || node.KeyFile != "" {
		if node.CertFile == "" || node.KeyFile == "" {
			return fmt.Errorf("certificateFile/keyFile must be provided together")
		}
		if _, err := tls.LoadX509KeyPair(node.CertFile, node.KeyFile); err != nil {
			return fmt.Errorf("invalid certificate files: %w", err)
		}
	}
	if tlsType == "tls" {
		if len(node.Certificates) == 0 && node.CertPEM == "" && node.CertFile == "" {
			return fmt.Errorf("tls_type=tls requires a local certificate/key")
		}
	}
	if node.ECHServerKeys != "" {
		if tlsType != "tls" {
			return fmt.Errorf("ECH server keys require tls_type=tls")
		}
		if strings.ContainsAny(node.ECHServerKeys, "\x00\r\n \t") {
			return fmt.Errorf("invalid ECH server keys base64")
		}
		raw, err := base64.StdEncoding.DecodeString(node.ECHServerKeys)
		if err != nil || len(raw) == 0 {
			return fmt.Errorf("invalid ECH server keys base64")
		}
	}
	if node.MLDSA65Seed != "" {
		if tlsType != "reality" {
			return fmt.Errorf("mldsa65_seed requires tls_type=reality")
		}
		seed, err := base64.RawURLEncoding.DecodeString(node.MLDSA65Seed)
		if err != nil || len(seed) != 32 {
			return fmt.Errorf("invalid reality mldsa65_seed (want base64url-encoded 32 bytes)")
		}
	}
	if dec := strings.TrimSpace(node.Decryption); dec != "" {
		if err := validateDecryption(dec); err != nil {
			return err
		}
	}
	if node.XHTTP != nil {
		switch node.XHTTP.Mode {
		case "", "auto", "packet-up", "stream-up", "stream-one":
		default:
			return fmt.Errorf("unsupported xhttp mode %q", node.XHTTP.Mode)
		}
		if strings.ContainsAny(node.XHTTP.Host, "\x00\r\n") {
			return fmt.Errorf("invalid xhttp host")
		}
		for k, v := range node.XHTTP.Headers {
			if strings.EqualFold(k, "host") {
				return fmt.Errorf("xhttp headers must not contain Host")
			}
			if strings.ContainsAny(k+v, "\x00\r\n") {
				return fmt.Errorf("invalid xhttp header %q", k)
			}
		}
	}
	if node.GRPC != nil {
		if node.GRPC.IdleTimeout < 0 || node.GRPC.HealthCheckTimeout < 0 || node.GRPC.InitialWindowsSize < 0 {
			return fmt.Errorf("grpc timeouts/window must be >= 0")
		}
		if strings.ContainsAny(node.GRPC.Authority+node.GRPC.UserAgent+node.GRPC.ServiceName, "\x00\r\n") {
			return fmt.Errorf("invalid grpc settings")
		}
	}
	for _, a := range node.ALPN {
		if a == "" || strings.ContainsAny(a, "\x00\r\n") {
			return fmt.Errorf("invalid ALPN value")
		}
	}
	switch node.MinVersion {
	case "", "1.2", "1.3":
	default:
		return fmt.Errorf("min_version must be 1.2 or 1.3")
	}

	if tlsType == "reality" {
		if strings.TrimSpace(node.PrivateKey) == "" {
			return fmt.Errorf("reality private_key is empty")
		}
		if len(node.ServerNames) == 0 {
			return fmt.Errorf("reality server_names is empty")
		}
		if node.Dest == "" {
			return fmt.Errorf("reality dest is empty")
		}
		if _, _, err := net.SplitHostPort(node.Dest); err != nil {
			return fmt.Errorf("reality dest must be host:port: %w", err)
		}
	}
	if tlsType == "reality" && len(node.ShortIDs) == 0 {
		// Xray rejects an empty shortIds list for REALITY ("empty shortIds").
		return fmt.Errorf("reality requires at least one short_id")
	}
	for _, sid := range node.ShortIDs {
		// An empty string is a valid, commonly used "disable shortId" value:
		// Xray hex-decodes it into an all-zero 8-byte id. Odd lengths and
		// non-hex characters are rejected by hex.DecodeString itself.
		if len(sid) > 16 {
			return fmt.Errorf("invalid short_id %q: too long", sid)
		}
		if _, err := hex.DecodeString(sid); err != nil {
			return fmt.Errorf("invalid short_id %q: %w", sid, err)
		}
	}
	for _, sn := range node.ServerNames {
		if sn == "" || len(sn) > 253 || strings.ContainsAny(sn, "\x00\r\n") {
			return fmt.Errorf("invalid server name %q", sn)
		}
	}
	return nil
}

func validateDecryption(dec string) error {
	if len(dec) > 8192 || strings.ContainsAny(dec, "\x00\r\n \t") {
		return fmt.Errorf("invalid VLESS decryption value")
	}
	if dec == "none" {
		return nil
	}
	parts := strings.Split(dec, ".")
	if len(parts) < 4 || parts[0] != "mlkem768x25519plus" {
		return fmt.Errorf("unsupported VLESS decryption value")
	}
	switch parts[1] {
	case "native", "xorpub", "random":
	default:
		return fmt.Errorf("unsupported VLESS decryption mode %q", parts[1])
	}
	if !strings.HasSuffix(parts[2], "s") {
		return fmt.Errorf("invalid VLESS decryption lifetime %q", parts[2])
	}
	t := strings.Split(strings.TrimSuffix(parts[2], "s"), "-")
	if len(t) > 2 {
		return fmt.Errorf("invalid VLESS decryption lifetime %q", parts[2])
	}
	from, err := parseNonNegInt(t[0])
	if err != nil {
		return fmt.Errorf("invalid VLESS decryption lifetime %q", parts[2])
	}
	if len(t) == 2 {
		to, err := parseNonNegInt(t[1])
		if err != nil || to < from {
			return fmt.Errorf("invalid VLESS decryption lifetime %q", parts[2])
		}
	}
	seenKey := false
	maxPaddingLen := 0
	for i, r := range parts[3:] {
		if len(r) < 20 {
			if seenKey {
				return fmt.Errorf("invalid VLESS decryption key segment")
			}
			if err := validateDecryptionPadding(i, r, &maxPaddingLen); err != nil {
				return err
			}
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(r)
		if err != nil || (len(b) != 32 && len(b) != 64) {
			return fmt.Errorf("invalid VLESS decryption key segment")
		}
		seenKey = true
	}
	if !seenKey {
		return fmt.Errorf("VLESS decryption contains no key segments")
	}
	return nil
}

func validateDecryptionPadding(idx int, seg string, maxLen *int) error {
	x := strings.Split(seg, "-")
	if len(x) != 3 {
		return fmt.Errorf("invalid VLESS decryption padding segment %q", seg)
	}
	var y [3]int
	for i := range y {
		v, err := parseNonNegInt(x[i])
		if err != nil {
			return fmt.Errorf("invalid VLESS decryption padding segment %q", seg)
		}
		y[i] = v
	}
	if y[2] < y[1] {
		return fmt.Errorf("invalid VLESS decryption padding range %q", seg)
	}
	if idx == 0 && (y[0] < 100 || y[1] < 35 || y[2] < 35) {
		return fmt.Errorf("invalid initial VLESS decryption padding segment %q", seg)
	}
	if idx%2 == 0 {
		*maxLen += y[2]
		if *maxLen > 18+65535 {
			return fmt.Errorf("VLESS decryption total padding length exceeds limit")
		}
	}
	return nil
}

func parseNonNegInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty integer")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("non-digit character")
		}
	}
	return strconv.Atoi(s)
}

// AuditChecker decides whether a connection must be blocked by audit rules.
type AuditChecker interface {
	Check(ctx context.Context, userID int64, dest xnet.Destination) bool
}

// ValidateUsers is the final safety net before handing users to Xray.
func ValidateUsers(users []User) error {
	ids := make(map[int64]struct{}, len(users))
	uuids := make(map[[16]byte]struct{}, len(users))
	for _, u := range users {
		if u.ID <= 0 {
			return fmt.Errorf("user id %d is invalid", u.ID)
		}
		if u.SpeedLimit < 0 || u.DeviceLimit < 0 || u.TCPLimit < 0 {
			return fmt.Errorf("user %d has a negative limit", u.ID)
		}
		if _, ok := ids[u.ID]; ok {
			return fmt.Errorf("duplicate user id %d", u.ID)
		}
		id, err := parseUUID(u.UUID)
		if err != nil {
			return fmt.Errorf("user %d UUID: %w", u.ID, err)
		}
		id[6], id[7] = 0, 0
		if _, ok := uuids[id]; ok {
			return fmt.Errorf("user %d UUID collides after Xray ProcessUUID normalization", u.ID)
		}
		ids[u.ID] = struct{}{}
		uuids[id] = struct{}{}
	}
	return nil
}
