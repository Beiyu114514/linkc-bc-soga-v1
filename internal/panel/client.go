package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Beiyu114514/linkc-bc-soga-v1/internal/store"
)

const (
	pathNode       = "/api/v1/node"
	pathUsers      = "/api/v1/users"
	pathAuditRules = "/api/v1/audit_rules"
	pathWhiteList  = "/api/v1/white_list"
	pathTraffic    = "/api/v1/traffic"
	pathAlive      = "/api/v1/alive_ip"
	pathAuditLog   = "/api/v1/audit_log"
	pathStatus     = "/api/v1/status"
)

// Cache is the ETag persistence interface (implemented by store.Store).
type Cache interface {
	GetETag(endpoint string) (store.ETagRecord, bool, error)
	PutETag(endpoint string, rec store.ETagRecord) error
}

// Options describes a soga-v1 panel client.
type Options struct {
	BaseURL         string
	APIKey          string
	NodeID          string
	NodeType        string
	Timeout         time.Duration
	VerifyTLS       bool
	CAFile          string
	ServerName      string
	ClientCertFile  string
	ClientKeyFile   string
	AllowHTTP       bool
	Proxy           string
	MaxResponseSize int64
	MaxAttempts     int
	UserAgent       string
	RetryBase       time.Duration
}

// Client talks to a soga-v1 panel.
type Client struct {
	cfg   Options
	base  string
	http  *http.Client
	cache Cache
}

// New constructs a client. cache may be nil (no ETag persistence).
func New(cfg Options, cache Cache) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("panel: base_url is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("panel: invalid base_url")
	}
	if u.Scheme != "https" && !cfg.AllowHTTP {
		return nil, fmt.Errorf("panel: https is required")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("panel: unsupported scheme %q", u.Scheme)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxResponseSize <= 0 {
		cfg.MaxResponseSize = 32 << 20
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.NodeType == "" {
		cfg.NodeType = "vless"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "linkc-node/dev"
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: !cfg.VerifyTLS, // explicit config; check warns in docs
		ServerName:         cfg.ServerName,
	}
	if cfg.CAFile != "" {
		b, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("panel: read CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("panel: CA file has no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCertFile != "" || cfg.ClientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("panel: client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	var proxy func(*http.Request) (*url.URL, error) = http.ProxyFromEnvironment
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("panel: invalid proxy URL")
		}
		proxy = http.ProxyURL(pu)
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsCfg,
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		cfg:   cfg,
		base:  strings.TrimRight(cfg.BaseURL, "/"),
		http:  hc,
		cache: cache,
	}, nil
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("panel %s: status %d: %s", e.Path, e.Status, e.Body)
}

func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

// FetchNode returns the node config and whether the body changed.
func (c *Client) FetchNode(ctx context.Context) (Node, bool, error) {
	var n Node
	changed, err := c.getJSON(ctx, pathNode, &n)
	return n, changed, err
}

// FetchUsers returns the user list and whether the body changed.
func (c *Client) FetchUsers(ctx context.Context) ([]User, bool, error) {
	var u []User
	changed, err := c.getJSON(ctx, pathUsers, &u)
	if u == nil {
		u = []User{}
	}
	return u, changed, err
}

// FetchAuditRules returns audit rules and whether the body changed.
func (c *Client) FetchAuditRules(ctx context.Context) ([]AuditRule, bool, error) {
	var rules []AuditRule
	changed, err := c.getJSON(ctx, pathAuditRules, &rules)
	if rules == nil {
		rules = []AuditRule{}
	}
	return rules, changed, err
}

// FetchWhiteList returns the panel audit whitelist.
func (c *Client) FetchWhiteList(ctx context.Context) ([]string, bool, error) {
	var items []string
	changed, err := c.getJSON(ctx, pathWhiteList, &items)
	if items == nil {
		items = []string{}
	}
	return items, changed, err
}

// PushAuditLog posts one batch of audit events.
func (c *Client) PushAuditLog(ctx context.Context, items []AuditLog) error {
	if len(items) == 0 {
		return nil
	}
	return c.postJSON(ctx, pathAuditLog, items)
}

// PushTraffic posts one traffic batch. The caller owns journal acknowledgement.
func (c *Client) PushTraffic(ctx context.Context, items []Traffic) error {
	if len(items) == 0 {
		return nil
	}
	return c.postJSON(ctx, pathTraffic, items)
}

// PushAliveIP posts current online IPs.
func (c *Client) PushAliveIP(ctx context.Context, items []AliveIP) error {
	if len(items) == 0 {
		return nil
	}
	return c.postJSON(ctx, pathAlive, items)
}

// PushStatus posts a status payload. mode is load_fraction or doc.
func (c *Client) PushStatus(ctx context.Context, st Status, mode string) error {
	load := st.CPUPercent / 100
	if load < 0 {
		load = 0
	} else if load > 1 {
		load = 1
	}
	payload := map[string]any{
		"cpu":    st.CPUPercent,
		"mem":    map[string]any{"total": st.MemTotal, "used": st.MemUsed},
		"swap":   map[string]any{"total": st.SwapTotal, "used": st.SwapUsed},
		"disk":   map[string]any{"total": st.DiskTotal, "used": st.DiskUsed},
		"uptime": st.Uptime,
	}
	if mode == "doc" {
		// Documented soga payload; the target SSPanel site needs load instead.
	} else {
		payload["load"] = fmt.Sprintf("%.6f", load)
	}
	return c.postJSON(ctx, pathStatus, payload)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) (changed bool, err error) {
	body, changed, err := c.get(ctx, path)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("panel %s: invalid json: %w", path, err)
	}
	return changed, nil
}

const (
	// maxRetryDelay caps the exponential backoff between attempts.
	maxRetryDelay = 5 * time.Minute
	// maxRetryAfterWait caps how long a single 429 may stall the caller. The
	// panel controls Retry-After, so an unbounded value would let one response
	// block config apply (and applyMu) for an arbitrary amount of time.
	maxRetryAfterWait = 60 * time.Second
)

func (c *Client) get(ctx context.Context, path string) ([]byte, bool, error) {
	var cached store.ETagRecord
	haveCache := false
	if c.cache != nil {
		rec, ok, err := c.cache.GetETag(c.cacheKey(path))
		if err == nil && ok {
			cached, haveCache = rec, true
		}
	}

	attempts := c.cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		body, changed, retry, wait, err := c.getOnce(ctx, path, cached, haveCache)
		if err == nil {
			return body, changed, nil
		}
		lastErr = err
		if !retry || attempt == attempts {
			// No further attempt: do not sleep, the caller gets the error now.
			break
		}
		delay := c.cfg.RetryBase * time.Duration(1<<uint(attempt-1))
		if wait > delay {
			delay = wait
		}
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		if !sleep(ctx, delay) {
			return nil, false, ctx.Err()
		}
	}
	return nil, false, lastErr
}

// getOnce performs one request. wait is the server-requested delay (429
// Retry-After); the caller decides whether another attempt is worth sleeping
// for.
func (c *Client) getOnce(ctx context.Context, path string, cached store.ETagRecord, haveCache bool) (body []byte, changed, retry bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, false, false, 0, err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json")
	if haveCache && cached.ETag != "" {
		req.Header.Set("IF-NONE-MATCH", cached.ETag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, true, 0, fmt.Errorf("panel %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		if !haveCache || len(cached.Body) == 0 {
			// Cache lost: retry unconditionally instead of returning a 304.
			b, changed, retry, wait, err := c.getOnceUnconditional(ctx, path)
			return b, changed, retry, wait, err
		}
		if c.cache != nil {
			cached.FetchedAt = time.Now()
			_ = c.cache.PutETag(c.cacheKey(path), cached)
		}
		return cached.Body, false, false, 0, nil
	case resp.StatusCode == http.StatusOK:
		limited := io.LimitReader(resp.Body, c.cfg.MaxResponseSize+1)
		b, err := io.ReadAll(limited)
		if err != nil {
			return nil, false, true, 0, fmt.Errorf("panel %s: read body: %w", path, err)
		}
		if int64(len(b)) > c.cfg.MaxResponseSize {
			return nil, false, false, 0, fmt.Errorf("panel %s: response exceeds %d bytes", path, c.cfg.MaxResponseSize)
		}
		if !json.Valid(b) {
			return nil, false, false, 0, fmt.Errorf("panel %s: invalid json response", path)
		}
		same := haveCache && len(cached.Body) > 0 && sha256.Sum256(b) == sha256.Sum256(cached.Body)
		rec := store.ETagRecord{
			ETag:      resp.Header.Get("ETag"),
			Body:      b,
			FetchedAt: time.Now(),
		}
		if c.cache != nil {
			// A cache write failure must not discard a valid response: the
			// read-only state DB (dump-config) and a full disk would otherwise
			// turn into a panel outage.
			_ = c.cache.PutETag(c.cacheKey(path), rec)
		}
		return b, !same, false, 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, false, true, clampRetryAfter(retryAfter(resp.Header.Get("Retry-After"))), &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	case resp.StatusCode >= 500:
		return nil, false, true, 0, &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	default:
		return nil, false, false, 0, &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	}
}

// getOnceUnconditional handles a 304 whose cached body is missing.
func (c *Client) getOnceUnconditional(ctx context.Context, path string) ([]byte, bool, bool, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, false, false, 0, err
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, true, 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, false, true, clampRetryAfter(retryAfter(resp.Header.Get("Retry-After"))), &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	case resp.StatusCode >= 500:
		return nil, false, true, 0, &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	default:
		return nil, false, false, 0, &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseSize+1))
	if err != nil {
		return nil, false, true, 0, err
	}
	if int64(len(b)) > c.cfg.MaxResponseSize {
		return nil, false, false, 0, fmt.Errorf("panel %s: response exceeds %d bytes", path, c.cfg.MaxResponseSize)
	}
	if !json.Valid(b) {
		return nil, false, false, 0, fmt.Errorf("panel %s: invalid json response", path)
	}
	rec := store.ETagRecord{ETag: resp.Header.Get("ETag"), Body: b, FetchedAt: time.Now()}
	if c.cache != nil {
		_ = c.cache.PutETag(c.cacheKey(path), rec)
	}
	return b, true, false, 0, nil
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("panel %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{Path: path, Status: resp.StatusCode, Body: readSmall(resp.Body)}
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseSize+1))
	if err != nil {
		return fmt.Errorf("panel %s: read body: %w", path, err)
	}
	if int64(len(respBody)) > c.cfg.MaxResponseSize {
		return fmt.Errorf("panel %s: response exceeds %d bytes", path, c.cfg.MaxResponseSize)
	}
	var result struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("panel %s: invalid response: %w", path, err)
	}
	if result.Code == nil {
		return fmt.Errorf("panel %s: response missing code", path)
	}
	if *result.Code != 0 {
		return fmt.Errorf("panel %s: code %d: %s", path, *result.Code, result.Message)
	}
	return nil
}

func (c *Client) cacheKey(path string) string {
	if c.cfg.NodeID == "" {
		return path
	}
	// Scope the cache to the node so changing panel.node_id cannot replay
	// another node's cached body via a 304.
	return c.cfg.NodeID + "|" + path
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("API-KEY", c.cfg.APIKey)
	req.Header.Set("NODE-ID", c.cfg.NodeID)
	req.Header.Set("NODE-TYPE", c.cfg.NodeType)
	req.Header.Set("User-Agent", c.cfg.UserAgent)
}

func readSmall(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4096))
	return strings.TrimSpace(string(b))
}

// clampRetryAfter bounds a server-supplied Retry-After. The panel controls
// this value, so an unbounded wait could block config apply indefinitely.
func clampRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfterWait {
		return maxRetryAfterWait
	}
	return d
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
