# M1 MVP 报告

> 对应规划：`PLAN.md` §15 M1
> 日期：2026-11-14
> 状态：**MVP 核心完成；等待真实站点联调**
> 代码：`cmd/linkc-node` + `internal/*`（生产骨架，不再依赖 `internal/m0` spike）

---

## 1. 结论

M1 的退出标准已达成：

- ✅ 真实 Xray 客户端（SOCKS + VLESS）→ 内嵌节点 → HTTP 目标，端到端跑通。
- ✅ mock 面板完成 `/node`、`/users`、`/traffic`、`/status` 契约联调。
- ✅ 面板完全断开后，节点继续接受**新连接**并提供代理。
- ✅ 进程重启时面板仍不可达，使用 `last_good` 从 bbolt 启动。
- ✅ 用户热移除/热恢复、空用户列表、ETag/304、流量 journal、状态上报。
- ✅ `go test ./...` 与 `go test -race ./...` 全部通过。
- ✅ Docker 镜像构建成功。

尚未完成（按计划属于 M2/M3/M4）：审计规则/白名单、alive_ip、XHTTP/GRPC 完整参数、ECH、多证书、真实站点联调。

---

## 2. 交付物

```
cmd/linkc-node/main.go        CLI: run/check/dump-config/version
internal/config/              YAML、环境变量、默认值、校验、证书检查
internal/store/               bbolt：etag/last_good/traffic journal/meta
internal/panel/               soga-v1 客户端：ETag、重试、traffic/status
internal/node/                NodeSpec/UserSpec 归一化、安全阀、指纹
internal/xray/                内嵌 Xray、配置生成、用户、stats、热切换、Gate
internal/traffic/             采集、journal、批量推送、ack
internal/status/              CPU/mem/swap/disk/uptime 采样
internal/agent/               装配、bootstrap、拉取/采集/推送循环、last_good
internal/admin/               Unix socket /healthz /readyz /status /metrics
configs/linkc-node.example.yaml
deploy/linkc-node.service
deploy/linkc-node@.service
Dockerfile / .dockerignore
docs/configuration.md
docs/operations.md
```

M0 spike 保留在 `internal/m0/*` 作为技术验证历史，生产路径不依赖它（仅测试复用 mock panel / Xray 客户端）。

---

## 3. 测试结果

```bash
export PATH=/usr/local/go/bin:$PATH
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/linkc-node
docker build -t linkc-node:m1 .
```

| 范围 | 结果 |
|---|---|
| `internal/agent` 端到端 | ✅ |
| 面板故障保持服务 + 新连接 | ✅ |
| last_good 重启（面板仍关闭） | ✅ |
| 用户热移除/恢复 | ✅ |
| 空 users 合法 | ✅ |
| `internal/panel` ETag/304、重试、POST | ✅ |
| `internal/store` journal/重开/ETag/last_good | ✅ |
| `internal/traffic` 采集/失败保留/ack/分片 | ✅ |
| `internal/xray` TCP、WS+TLS、REALITY 配置矩阵 | ✅ |
| `internal/xray` WS+TLS 实际连接 | ✅ |
| `internal/config` 默认值/环境变量/严格字段 | ✅ |
| `internal/admin` Unix socket | ✅ |
| `-race` 全量 | ✅ |
| Docker 构建/运行 version | ✅ |

关键 E2E 证据：

- `TestAgentEndToEndAndPanelOutage`：客户端经节点访问目标；面板收到 traffic/status；关闭面板后**新启动的 Xray 客户端**仍可访问；`degraded=true`；用同一 state DB 重启，面板仍关闭，节点从 last_good 启动并继续服务。
- `TestAgentUserHotUpdate`：面板删除用户后新连接失败；重新加回后恢复。
- `TestAgentEmptyUsersIsLegal`：`[]` 被正常应用。
- `TestStartWSTLS`：生产 `internal/xray` 的 WS + TLS 实际握手/传输。
- `TestBuildConfigMatrix`：tcp/ws/reality、h2 拒绝、vision 组合校验。

---

## 4. 关键实现

### 4.1 启动与 last_good

1. 打开 bbolt（0600，目录 0700）。
2. 读 `last_good/node` + `last_good/users`。
3. 有缓存直接启动 Xray；缺哪个拉哪个。
4. 无缓存且首次拉取失败 → 退出，由 systemd 重试。
5. 启动成功后立即进入拉取/采集/推送/状态循环。

面板不可达时不清空用户、不关闭核心；连续 3 次拉取失败进入 `degraded`。恢复后下一次 pull 自动追平。

### 4.2 配置应用

- `/node` 的 ETag/304、body SHA-256 相同 → 不触发应用。
- 只有 `basic`（间隔/限速）变化 → 更新内存/限速，不碰 inbound。
- inbound 字段变化 → `AddInbound` 新 tag → `RemoveInbound` 旧 tag；依赖 Xray SO_REUSEPORT。
- 用户变化 → `proxy.UserManager` 增量 Add/Remove；UUID 变化先删后加。
- 用户指纹包含 `uuid/speed/device/tcp`，限额变化也会更新 Gate。
- 空 users 是合法业务状态，默认应用；`mass_remove_guard` 可配置 `warn|reject`。

### 4.3 流量 journal

- 采集：`counter.Set(0)` 读取增量 → `AppendTraffic` 单事务 fsync。
- 写盘失败：增量留在内存，下一次采集先尝试补齐，期间不再读取新 counter。
- 推送：按 seq 聚合用户 → 分片 POST → 成功后删除 journal 记录。
- 失败保留；重复风险为 at-least-once（站点无幂等键）。
- journal 超过 `max_journal_bytes` 暂停采集并告警，代理继续服务。

### 4.4 状态上报

- `/proc/stat` 差分 CPU；`/proc/meminfo`、`statfs(storage.path)`、`/proc/uptime`。
- 默认 `status.mode=load_fraction`：`load="0.235000"` + `cpu/mem/swap/disk/uptime` 双字段。
- 目标站点前端 `load[0]*100` 得到 23.5%。

### 4.5 安全/配置

- YAML 严格字段（未知字段报错）；`${ENV}` 展开；`api_key_file`。
- 面板 HTTPS 默认严格验证；禁止重定向；API key 不进日志。
- 节点 TLS 证书读取为 inline PEM，规避 Xray 文件证书热加载的上游 data race。
- `freedom.allow_private_targets` 默认 false。
- CLI `check` 校验证书/密钥/目录；`dump-config` 脱敏输出。
- `admin.sock` 0600；systemd 使用 `CAP_NET_BIND_SERVICE`、`ProtectSystem=strict`。

---

## 5. M1 任务对照

| M1 任务 | 状态 | 说明 |
|---|---|---|
| 本地配置、校验、CLI、日志、bbolt | ✅ | `config`/`store`/`cmd` |
| Panel Client GET node/users、ETag、重试；POST traffic/status | ✅ | `panel` |
| `sspanel-uim-soga` profile | ✅ | 默认值/空 users/load_fraction |
| NodeSpec → Xray（TCP/WS/TLS/REALITY） | ✅ | `node`/`xray`，WS+TLS E2E |
| status 双字段（load + cpu/mem） | ✅ | `status`/`panel` |
| 用户同步 diff/安全阀 | ✅ | `node.NormalizeUsers` + `Engine.ApplyUsers` |
| 流量采集/journal/推送 | ✅ | `traffic`，at-least-once |
| 状态上报 | ✅ | 周期采样 + push |
| systemd unit、Dockerfile | ✅ | 已构建验证 |
| 基础文档 | ✅ | `docs/*` |

---

## 6. 已知限制 / 遗留

1. **真实站点联调**：仍需要线上测试节点/API key；`status.load`、审计裸 regex、证书来源未在真实 soga 上复验。
2. **审计规则/白名单**：M2；当前配置字段保留，Gate 未执行审计。
3. **alive_ip 拉取/推送**：M2；`Engine.OnlineIPs()` 已就绪。
4. **XHTTP/GRPC/ECH/decryption**：M2；M1 只完整验证 TCP/WS+TLS/REALITY 配置与 TCP/WS 实际连接。
5. **`traffic.include_online_zero`**：字段保留，M1 未实现；当前站点在线数是“本轮有流量用户数”的近似。
6. **多证书/SNI**：MVP 仅一张证书；多证书需要扩展 `NodeConfig`。
7. **证书热加载/SIGHUP**：未实现；证书变更需重启。
8. **端口占用回滚**：同端口先加后删；失败保留旧 inbound，尚未实现“先删旧再加新”的完整回滚（M4）。
9. **Gatekeeper**：enforce/report/off 基础模式已有；设备 IPv6 /64 聚合、审计阻断未实现。
10. **代理**：面板 HTTP 代理可用，SOCKS 代理未接入。
11. **生产 import**：仍使用 `main/distro/all`，M4 可裁剪。

---

## 7. 运行

```bash
export PATH=/usr/local/go/bin:$PATH
go build -o linkc-node ./cmd/linkc-node
PANEL_API_KEY=... ./linkc-node check -c configs/linkc-node.example.yaml
PANEL_API_KEY=... ./linkc-node run -c configs/linkc-node.example.yaml
```

管理面：

```bash
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
```

---

## 8. 下一步（M2）

1. 真实站点联调，核对 `status.load`、空 users、traffic 在线数、审计裸 regex。
2. `audit_rules`/`white_list` 拉取、解析（report 模式）、事件队列/自然日去重。
3. `alive_ip` 采集与 `changed/full` 推送。
4. XHTTP/GRPC、ECH、decryption；错误矩阵与分片。
5. 崩溃恢复/对账/admin 扩展/指标告警。
