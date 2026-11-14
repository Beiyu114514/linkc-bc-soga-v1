# 配置说明（M1）

配置文件默认 `/etc/linkc-node/config.yaml`，示例见 `configs/linkc-node.example.yaml`。
所有标量支持 `${ENV_NAME}` 环境变量展开；未设置会直接报错。展开在 YAML 解析后进行，含特殊字符的值不会被破坏或注入配置（字符串值建议加引号，未加引号的 `${PORT}` 会按数字解析）。

## panel

| 字段 | 默认 | 说明 |
|---|---|---|
| `base_url` | — | soga-v1 面板根地址，路径必须与站点授权路径一致 |
| `profile` | `sspanel-uim-soga` | 目标站点 profile |
| `api_key` / `api_key_file` | — | `API-KEY`；文件方式适合 systemd `LoadCredential` 或密钥挂载 |
| `node_id` | — | `NODE-ID` |
| `node_type` | `vless` | 固定 VLESS，其他值拒绝 |
| `timeout` | `15s` | 单次 HTTP 超时 |
| `allow_http` | `false` | 仅测试/内网允许 |
| `follow_redirects` | `false` | 固定禁止，避免 API key 泄露 |
| `tls.*` | verify=true | 自定义 CA、SNI、mTLS |
| `proxy` | 空 | 可选 HTTP 代理，例如 `http://127.0.0.1:7890` |
| `max_attempts` | `3` | GET 重试次数；401/403 不重试 |

## node

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `0.0.0.0` | 面板只给端口，监听地址本地决定 |
| `pull_interval` | `60s` | 面板 `basic.pull_interval` 优先 |
| `push_interval` | `60s` | 面板 `basic.push_interval` 优先 |
| `collect_interval` | `30s` | 必须 ≤ 推送间隔（建议） |
| `status_interval` | `5s` | 本机状态采样 |
| `tls.certificates` | 空 | `tls_type=tls` 时必需；支持多组，Xray 按 SNI 选择 |
| `tls.alpn` | 空 | 例如 `["h2","http/1.1"]` |
| `tls.min_version` | 空 | `1.2` / `1.3` |
| `freedom.allow_private_targets` | `false` | 生产保持 false，阻止 SSRF |
| `limits.mode` | `enforce` | `enforce` / `report` / `off` |
| `users.allow_empty` | `true` | 站点全员超流量/节点超带宽会返回 `[]` |
| `users.mass_remove_guard` | `warn` | 大量删除只告警；`reject` 严格 |
| `traffic.batch_users` | `500` | 单片用户数 |
| `traffic.batch_bytes` | `1MiB` | 单片字节上限 |
| `traffic.include_online_zero` | `false` | 额外发送 0/0 在线用户，准确化站点 `online_user` |
| `alive_ip.mode` | `changed` | `changed` 只发变更；`full` 每次全量 |
| `alive_ip.full_every` | `20` | changed 模式下每 N 次补一次全量 |
| `status.mode` | `load_fraction` | `load` = CPU 小数（0..1）；`doc` 不兼容目标站点 |

### XHTTP / GRPC / ECH / decryption（本地扩展）

目标站点当前不下发 `xhttp_settings`、`grpc_settings`、`ech_key`、`decryption`。本程序优先使用面板字段；缺省时使用本地配置：

```yaml
node:
  vless:
    decryption: "none"
  xhttp:
    host: "cdn.example.com"
    path: "/x"
    mode: "auto"          # auto | packet-up | stream-up | stream-one
    headers: {}
  grpc:
    authority: ""
    service_name: ""
    multi_mode: false
    idle_timeout: 0
    health_check_timeout: 0
    permit_without_stream: false
    initial_windows_size: 0
    user_agent: ""
  tls:
    ech_server_keys: ""   # base64 ECHKeySetList；仅 tls
```

- XHTTP/GRPC 仅在面板 `stream_type` 对应时生效。
- ECH 仅支持 `tls_type=tls`，必须提供合法 base64 ECH key set。
- `decryption` 默认 `none`；非 `none` 需要 Xray 支持的 VLESS 加密参数。
- gRPC 在当前 Xray 已弃用，建议优先 XHTTP。

### 证书与 REALITY

- TLS 证书从本地文件读取到内存后以 inline PEM 注入 Xray；不依赖 Xray 的文件证书热加载（该路径存在上游 data race）。
- REALITY 的 `private_key`、`short_ids`、`server_names`、`dest` 来自面板；`xver` 与可选的 `mldsa65_seed` 可由本地 `node.reality.*` 覆盖。
- `tls_type=xtls` 会被拒绝并提示改用 `tls`/`reality`；目标站点会把 xtls 映射为 none。

### 限额模式

- `enforce`：执行用户/节点限速、设备数、TCP 限制（Gatekeeper）。
- `report`：只计数不拒绝（M1 只保留基础计数）。
- `off`：完全关闭 Gatekeeper 限制。
- 限速连接会设置 `CanSpliceCopy=3`，关闭 Vision 零拷贝，否则限速会被绕过。

## audit

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `true` | 是否拉取并执行审计规则 |
| `action` | `block` | `block` 阻断连接；`report` 只上报 |
| `rule_mode` | `auto` | 已知前缀优先；裸值含明确正则元字符才按 regex |
| `whitelist_file` | 空 | 本地白名单，每行一条，支持 `#` 注释 |
| `dedupe_ttl` | `24h` | 本地 `(user, rule)` 去重窗口 |
| `strict_rules` | `false` | true 时任一规则编译失败则整批拒绝 |
| `resolve_domain_for_ip_rules` | `false` | 域名目标上的 `ip:`/`geoip:` 规则本地解析 |

规则语法：

- 前缀：`regexp:` / `domain:` / `full:` / `keyword:` / `ip:` / `port:`
- 站点裸值：`auto` 模式下无正则元字符按 keyword，含 `\* + ? [ ] ( ) { } | ^ $` 按 regex
- 白名单优先；面板列表 + 本地文件合并
- `geosite:` / `geoip:` 通过 `node.geodata.dir` 加载 `geosite.dat` / `geoip.dat`；文件缺失时跳过并告警
- 阻断在 Gatekeeper 出站入口执行；命中后按 `(user, rule)` 去重入 bbolt 队列，推送 `/audit_log`
- 审计依赖 sniffing；纯 IP、ECH/ESNI、HTTP/3 可能看不到域名

## storage / logging / admin

- `storage.path`：bbolt 目录，自动 0700；`state.db` 0600。
- `storage.max_journal_bytes`：未确认流量 journal 上限，达到后暂停采集并告警。
- `logging.format`：`json`（生产）或 `text`（调试）。
- `admin.socket`：Unix socket，0600；提供 `/healthz`、`/readyz`、`/status`、`/metrics`。
- `admin.metrics=false` 时不注册 `/metrics`。
- `admin.pprof=true` 时额外注册 `/debug/pprof/*`（仅本地 socket 可达）。
