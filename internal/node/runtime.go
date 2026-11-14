package node

import (
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

// LocalRuntimeFromConfig builds the local-only runtime settings from a
// validated config, loaded certificate material and an optional audit hook.
// Both the agent and the `dump-config` CLI use it so their output cannot drift.
func LocalRuntimeFromConfig(cfg config.Config, certs []xray.TLSCertificate, audit xray.AuditChecker) LocalRuntime {
	return LocalRuntime{
		Listen:           cfg.Node.Listen,
		Certificates:     append([]xray.TLSCertificate(nil), certs...),
		AllowPrivate:     cfg.Node.Freedom.AllowPrivateTargets,
		Sniffing:         cfg.Node.Sniffing.Mode != "off",
		LimitsMode:       cfg.Node.Limits.Mode,
		BurstBytes:       cfg.Node.Limits.BurstBytes,
		DeviceIPv6Prefix: cfg.Node.Limits.DeviceLimitIPv6Prefix,
		ALPN:             append([]string(nil), cfg.Node.TLS.ALPN...),
		MinVersion:       cfg.Node.TLS.MinVersion,
		ECHServerKeys:    cfg.Node.TLS.ECHServerKeys,
		Decryption:       cfg.Node.VLESS.Decryption,
		RealityXVER:      cfg.Node.Reality.XVER,
		MLDSA65Seed:      cfg.Node.Reality.MLDSA65Seed,
		XHTTP:            localXHTTP(cfg.Node.XHTTP),
		GRPC:             localGRPC(cfg.Node.GRPC),
		Audit:            audit,
	}
}

func localXHTTP(c config.XHTTPConfig) *xray.XHTTPConfig {
	if c.Host == "" && c.Path == "" && c.Mode == "" && len(c.Headers) == 0 {
		return nil
	}
	return &xray.XHTTPConfig{Host: c.Host, Path: c.Path, Mode: c.Mode, Headers: c.Headers}
}

func localGRPC(c config.GRPCConfig) *xray.GRPCConfig {
	if c.Authority == "" && c.ServiceName == "" && !c.MultiMode && c.IdleTimeout == 0 &&
		c.HealthCheckTimeout == 0 && !c.PermitWithoutStream && c.InitialWindowsSize == 0 && c.UserAgent == "" {
		return nil
	}
	return &xray.GRPCConfig{Authority: c.Authority, ServiceName: c.ServiceName, MultiMode: c.MultiMode,
		IdleTimeout: c.IdleTimeout, HealthCheckTimeout: c.HealthCheckTimeout,
		PermitWithoutStream: c.PermitWithoutStream, InitialWindowsSize: c.InitialWindowsSize, UserAgent: c.UserAgent}
}
