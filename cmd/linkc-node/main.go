package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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
	sockPath := fs.String("s", "", "admin unix socket path (overrides config admin.socket)")
	fs.StringVar(sockPath, "socket", "", "admin unix socket path (overrides config admin.socket)")
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
	case "status":
		return printAdminEndpoint(*cfgPath, *sockPath, "/status", true)
	case "metrics":
		return printAdminEndpoint(*cfgPath, *sockPath, "/metrics", false)
	case "ready":
		return printAdminEndpoint(*cfgPath, *sockPath, "/readyz", false)
	case "run":
		return runAgent(*cfgPath)
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() error {
	fmt.Fprint(os.Stderr, `usage: linkc-node <run|check|dump-config|status|metrics|ready|version> [-c config.yaml]

  run          start the node agent
  check        validate local config and certificate files
  dump-config  fetch panel config and print the generated Xray JSON (redacted)
  status       query node status via admin Unix socket
  metrics      query Prometheus metrics via admin Unix socket
  ready        check readiness via admin Unix socket
  version      print version
`)
	return nil
}

func resolveAdminSocket(cfgPath, sockPath string) string {
	if s := strings.TrimSpace(sockPath); s != "" {
		return s
	}
	if cfg, err := config.Load(cfgPath); err == nil && strings.TrimSpace(cfg.Admin.Socket) != "" {
		return cfg.Admin.Socket
	}
	return "/run/linkc-node/admin.sock"
}

func printAdminEndpoint(cfgPath, sockPath, endpoint string, prettyJSON bool) error {
	body, err := queryAdmin(resolveAdminSocket(cfgPath, sockPath), endpoint)
	if err != nil {
		return err
	}
	if prettyJSON {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err == nil {
			fmt.Println(buf.String())
			return nil
		}
	}
	fmt.Print(string(body))
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func queryAdmin(socketPath, endpoint string) ([]byte, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, fmt.Errorf("empty admin socket path")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	resp, err := client.Get("http://localhost" + endpoint)
	if err != nil {
		return nil, fmt.Errorf("query %s via %s: %w", endpoint, socketPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin %s returned HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
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
	// The reload goroutine must be joined before the deferred a.Close() tears
	// down the store and Xray, otherwise an in-flight ReloadCerts can race
	// with shutdown and touch a closed DB or engine.
	hupCtx, stopHup := context.WithCancel(ctx)
	var hupWG sync.WaitGroup
	hupWG.Add(1)
	go func() {
		defer hupWG.Done()
		for {
			select {
			case <-hup:
				if err := a.ReloadCerts(); err != nil {
					log.Error("reload certificates", "error", err)
				}
			case <-hupCtx.Done():
				return
			}
		}
	}()
	defer func() {
		stopHup()
		hupWG.Wait()
	}()
	var adm *admin.Server
	if cfg.Admin.Socket != "" {
		adm, err = admin.Start(cfg.Admin.Socket, a.Snapshot, admin.Options{Metrics: cfg.Admin.Metrics, Pprof: cfg.Admin.Pprof})
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
	var st *store.Store
	var cache panel.Cache
	// Try to reuse last_good from the state DB. A running agent holds the
	// bbolt write lock, so a failed read-only open is expected and we simply
	// fall back to fetching everything from the panel.
	if _, statErr := os.Stat(cfg.StateDBPath()); statErr == nil {
		if s, openErr := store.OpenReadOnly(cfg.StateDBPath()); openErr == nil {
			st = s
			defer st.Close()
			cache = st
		}
	}
	ua := cfg.Panel.UserAgent
	if ua == "" || ua == "linkc-node/dev" {
		ua = version.UserAgent()
	}
	pc, err := panel.New(panel.Options{
		BaseURL: cfg.Panel.BaseURL, APIKey: cfg.Panel.APIKey, NodeID: cfg.Panel.NodeID,
		NodeType: cfg.Panel.NodeType, Timeout: cfg.Panel.Timeout.Duration(),
		VerifyTLS: cfg.Panel.TLS.Verify, CAFile: cfg.Panel.TLS.CAFile,
		ServerName: cfg.Panel.TLS.ServerName, ClientCertFile: cfg.Panel.TLS.ClientCertFile,
		ClientKeyFile: cfg.Panel.TLS.ClientKeyFile, AllowHTTP: cfg.Panel.AllowHTTP,
		Proxy: cfg.Panel.Proxy, MaxResponseSize: cfg.Panel.MaxResponseSize,
		MaxAttempts: cfg.Panel.MaxAttempts, UserAgent: ua,
	}, cache)
	if err != nil {
		return err
	}
	var pn panel.Node
	ctx := context.Background()
	if n, _, err := pc.FetchNode(ctx); err == nil {
		pn = n
	} else if st == nil {
		return fmt.Errorf("fetch node: %w", err)
	} else if ok, _ := st.GetLastGood(agent.LastGoodKey(cfg.Panel.NodeID, "node"), &pn); !ok {
		return fmt.Errorf("fetch node: %w", err)
	}
	var pu []panel.User
	if u, _, err := pc.FetchUsers(ctx); err == nil {
		pu = u
	} else if st == nil {
		return fmt.Errorf("fetch users: %w", err)
	} else if ok, _ := st.GetLastGood(agent.LastGoodKey(cfg.Panel.NodeID, "users"), &pu); !ok {
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
	xc, err := spec.Xray(node.LocalRuntimeFromConfig(cfg, certs, nil))
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

func redact(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch strings.ToLower(k) {
			case "id", "email", "privatekey", "private_key", "certificate", "key",
				"shortids", "short_ids", "headers",
				"echserverkeys", "ech_server_keys", "mldsa65seed", "mldsa65_seed":
				x[k] = "***"
			case "decryption":
				if s, ok := val.(string); ok {
					if trimmed := strings.TrimSpace(s); trimmed != "" && !strings.EqualFold(trimmed, "none") {
						x[k] = "***"
					}
				} else if val != nil {
					x[k] = "***"
				}
			default:
				redact(val)
			}
		}
	case []any:
		for _, val := range x {
			redact(val)
		}
	}
}
