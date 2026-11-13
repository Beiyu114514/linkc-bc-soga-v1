package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/admin"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/agent"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/config"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/node"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/panel"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/version"
	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/xray"
)

func main() {
	syscall.Umask(0o077)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "linkc-node:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("c", "/etc/linkc-node/config.yaml", "config file")
	fs.StringVar(cfgPath, "config", *cfgPath, "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch cmd {
	case "version":
		fmt.Printf("linkc-node %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return nil
	case "check":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		if err := cfg.Check(); err != nil {
			return err
		}
		fmt.Println("config ok")
		return nil
	case "dump-config":
		return dumpConfig(*cfgPath)
	case "run":
		return runAgent(*cfgPath)
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() error {
	fmt.Fprint(os.Stderr, `usage: linkc-node <run|check|dump-config|version> [-c config.yaml]

  run          start the node agent
  check        validate local config and certificate files
  dump-config  fetch panel config and print the generated Xray JSON (redacted)
  version      print version
`)
	return nil
}

func loggerFor(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.Logging.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Logging.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func runAgent(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	log := loggerFor(cfg)
	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-hup:
				if err := a.ReloadCerts(); err != nil {
					log.Error("reload certificates", "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	var adm *admin.Server
	if cfg.Admin.Socket != "" {
		adm, err = admin.Start(cfg.Admin.Socket, a.Snapshot)
		if err != nil {
			return err
		}
		defer adm.Close()
	}
	log.Info("starting", "version", version.Version, "node_id", cfg.Panel.NodeID)
	return a.Run(ctx)
}

func dumpConfig(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.Check(); err != nil {
		return err
	}
	st, err := store.Open(cfg.StateDBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	pc, err := panel.New(panel.Options{
		BaseURL: cfg.Panel.BaseURL, APIKey: cfg.Panel.APIKey, NodeID: cfg.Panel.NodeID,
		NodeType: cfg.Panel.NodeType, Timeout: cfg.Panel.Timeout.Duration(),
		VerifyTLS: cfg.Panel.TLS.Verify, CAFile: cfg.Panel.TLS.CAFile,
		ServerName: cfg.Panel.TLS.ServerName, ClientCertFile: cfg.Panel.TLS.ClientCertFile,
		ClientKeyFile: cfg.Panel.TLS.ClientKeyFile, AllowHTTP: cfg.Panel.AllowHTTP,
		Proxy: cfg.Panel.Proxy, MaxResponseSize: cfg.Panel.MaxResponseSize,
		MaxAttempts: cfg.Panel.MaxAttempts, UserAgent: cfg.Panel.UserAgent,
	}, st)
	if err != nil {
		return err
	}
	var pn panel.Node
	ctx := context.Background()
	if n, _, err := pc.FetchNode(ctx); err == nil {
		pn = n
	} else if ok, _ := st.GetLastGood("node", &pn); !ok {
		return fmt.Errorf("fetch node: %w", err)
	}
	var pu []panel.User
	if u, _, err := pc.FetchUsers(ctx); err == nil {
		pu = u
	} else if ok, _ := st.GetLastGood("users", &pu); !ok {
		return fmt.Errorf("fetch users: %w", err)
	}
	spec, err := node.FromPanel(pn)
	if err != nil {
		return err
	}
	certs, err := loadCerts(cfg)
	if err != nil {
		return err
	}
	xc, err := spec.Xray(node.LocalRuntime{
		Listen: cfg.Node.Listen, Certificates: certs,
		AllowPrivate: cfg.Node.Freedom.AllowPrivateTargets,
		Sniffing:     cfg.Node.Sniffing.Mode != "off",
		LimitsMode:   cfg.Node.Limits.Mode, BurstBytes: cfg.Node.Limits.BurstBytes,
	})
	if err != nil {
		return err
	}
	users, _, err := node.NormalizeUsers(pu, node.UserOptions{
		AllowEmpty: cfg.Node.Users.AllowEmpty, MassRemoveGuard: cfg.Node.Users.MassRemoveGuard,
		MaxUsers: cfg.Node.Users.MaxUsers,
	}, nil)
	if err != nil {
		return err
	}
	raw, err := xray.BuildConfig(xc, users, "vless-1")
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return err
	}
	redact(v)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func loadCerts(cfg config.Config) ([]xray.TLSCertificate, error) {
	out := make([]xray.TLSCertificate, 0, len(cfg.Node.TLS.Certificates))
	for _, cp := range cfg.Node.TLS.Certificates {
		c, err := os.ReadFile(cp.CertFile)
		if err != nil {
			return nil, err
		}
		k, err := os.ReadFile(cp.KeyFile)
		if err != nil {
			return nil, err
		}
		out = append(out, xray.TLSCertificate{CertPEM: string(c), KeyPEM: string(k)})
	}
	return out, nil
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

func redact(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch k {
			case "id", "email", "privateKey", "private_key", "certificate", "key", "shortIds", "short_ids":
				x[k] = "***"
			default:
				redact(val)
			}
		}
	case []any:
		for _, val := range x {
			redact(val)
		}
	case string:
		if strings.Contains(x, "BEGIN ") {
			// handled by key, defensive
		}
	}
}
