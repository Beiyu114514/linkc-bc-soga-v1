// Package panel implements the soga-v1 WebAPI client.
package panel

// Node is the normalized GET /api/v1/node payload. Unknown fields are ignored.
type Node struct {
	Basic       Basic   `json:"basic"`
	Config      Config  `json:"config"`
	NodeType    string  `json:"node_type"`
	TrafficRate float64 `json:"traffic_rate"`
}

type Basic struct {
	PullInterval int     `json:"pull_interval"`
	PushInterval int     `json:"push_interval"`
	SpeedLimit   float64 `json:"speed_limit"`
}

// Config is the VLESS node config subset returned by the target site.
type Config struct {
	Port        int          `json:"port"`
	StreamType  string       `json:"stream_type"`
	TLSType     string       `json:"tls_type"`
	Flow        string       `json:"flow"`
	ServerNames []string     `json:"server_names"`
	PrivateKey  string       `json:"private_key"`
	ShortIDs    []string     `json:"short_ids"`
	Dest        string       `json:"dest"`
	Path        string       `json:"path"`
	ServiceName string       `json:"service_name"`
	XVER        int          `json:"xver"`
	Decryption  string       `json:"decryption"`
	ECHKey      string       `json:"ech_key"`
	XHTTP       *XHTTPConfig `json:"xhttp_settings"`
	GRPC        *GRPCConfig  `json:"grpc_settings"`
}

// XHTTPConfig mirrors the useful subset of Xray xhttpSettings.
type XHTTPConfig struct {
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Mode    string            `json:"mode"`
	Headers map[string]string `json:"headers"`
}

// GRPCConfig mirrors the useful subset of Xray grpcSettings.
type GRPCConfig struct {
	Authority           string `json:"authority"`
	ServiceName         string `json:"service_name"`
	MultiMode           bool   `json:"multi_mode"`
	IdleTimeout         int32  `json:"idle_timeout"`
	HealthCheckTimeout  int32  `json:"health_check_timeout"`
	PermitWithoutStream bool   `json:"permit_without_stream"`
	InitialWindowsSize  int32  `json:"initial_windows_size"`
	UserAgent           string `json:"user_agent"`
}

// User is the subset of GET /api/v1/users that affects VLESS.
type User struct {
	ID          int64   `json:"id"`
	UUID        string  `json:"uuid"`
	SpeedLimit  float64 `json:"speed_limit"`
	DeviceLimit int     `json:"device_limit"`
	TCPLimit    int     `json:"tcp_limit"`
}

// AuditRule is one GET /api/v1/audit_rules item.
type AuditRule struct {
	ID   int64  `json:"id"`
	Rule string `json:"rule"`
}

// AuditLog is one POST /api/v1/audit_log item.
type AuditLog struct {
	UserID  int64 `json:"user_id"`
	AuditID int64 `json:"audit_id"`
}

// Traffic is one POST /api/v1/traffic item.
type Traffic struct {
	ID int64 `json:"id"`
	U  int64 `json:"u"`
	D  int64 `json:"d"`
}

// AliveIP is one POST /api/v1/alive_ip item.
type AliveIP struct {
	ID  int64    `json:"id"`
	IPs []string `json:"ips"`
}

// Status is the host status payload.
type Status struct {
	CPUPercent float64
	MemTotal   int64
	MemUsed    int64
	SwapTotal  int64
	SwapUsed   int64
	DiskTotal  int64
	DiskUsed   int64
	Uptime     int64
}
