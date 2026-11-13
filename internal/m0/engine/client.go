package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/core"
	confserial "github.com/xtls/xray-core/infra/conf/serial"
)

// ClientConfig is a minimal VLESS client for M0 e2e tests.
type ClientConfig struct {
	SocksPort            int
	Server               string
	Port                 int
	UUID                 string
	Flow                 string
	Network              string // raw (default), ws, grpc, xhttp
	Path                 string
	XHTTPMode            string
	XHTTPHost            string
	ServiceName          string
	TLS                  bool
	ServerName           string
	PinnedPeerCertSha256 string

	Reality     bool
	PublicKey   string
	ShortID     string
	Fingerprint string
}

// StartClient starts a second in-process Xray instance with a SOCKS inbound
// and a VLESS outbound, used only by tests.
func StartClient(ctx context.Context, cfg ClientConfig) (*core.Instance, error) {
	network := cfg.Network
	if network == "" {
		network = "raw"
	}
	security := "none"
	stream := map[string]any{"network": network, "security": security}
	switch network {
	case "ws":
		stream["wsSettings"] = map[string]any{"path": cfg.Path}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": cfg.ServiceName}
	case "xhttp":
		xh := map[string]any{"path": cfg.Path}
		if cfg.XHTTPMode != "" {
			xh["mode"] = cfg.XHTTPMode
		}
		if cfg.XHTTPHost != "" {
			xh["host"] = cfg.XHTTPHost
		}
		stream["xhttpSettings"] = xh
	}
	switch {
	case cfg.Reality:
		fp := cfg.Fingerprint
		if fp == "" {
			fp = "chrome"
		}
		stream["security"] = "reality"
		stream["realitySettings"] = map[string]any{
			"serverName":  cfg.ServerName,
			"fingerprint": fp,
			"publicKey":   cfg.PublicKey,
			"shortId":     cfg.ShortID,
			"spiderX":     "/",
		}
	case cfg.TLS:
		stream["security"] = "tls"
		tls := map[string]any{}
		if cfg.ServerName != "" {
			tls["serverName"] = cfg.ServerName
		}
		if cfg.PinnedPeerCertSha256 != "" {
			tls["pinnedPeerCertSha256"] = cfg.PinnedPeerCertSha256
		}
		stream["tlsSettings"] = tls
	}
	outUser := map[string]any{"id": cfg.UUID, "encryption": "none"}
	if cfg.Flow != "" {
		outUser["flow"] = cfg.Flow
	}
	payload := map[string]any{
		"log": map[string]any{"loglevel": "warning", "access": "none"},
		"inbounds": []any{
			map[string]any{
				"tag": "socks-in", "listen": "127.0.0.1", "port": cfg.SocksPort,
				"protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": false},
			},
		},
		"outbounds": []any{
			map[string]any{
				"tag": "vless-out", "protocol": "vless",
				"settings": map[string]any{
					"vnext": []any{map[string]any{
						"address": cfg.Server,
						"port":    cfg.Port,
						"users":   []any{outUser},
					}},
				},
				"streamSettings": stream,
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	conf, err := confserial.LoadJSONConfig(strings.NewReader(string(b)))
	if err != nil {
		return nil, fmt.Errorf("client config: %w", err)
	}
	inst, err := core.New(conf)
	if err != nil {
		return nil, err
	}
	if err := inst.Start(); err != nil {
		_ = inst.Close()
		return nil, err
	}
	return inst, nil
}
