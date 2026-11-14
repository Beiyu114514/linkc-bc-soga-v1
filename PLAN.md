# LinkC Node · 基于 Xray-core 的 soga-v1 节点后端

> **文档性质**：技术规划（Design / Plan），**不包含实现代码**
> **版本**：v0.1 Draft
> **日期**：2026-11-14
> **仓库**：`Beiyu114514/linkc-bc-soga-v1`
> **协议依据**：[soga-v1 webapi 开发文档](https://www.yougotme.cc/soga/doc/sogav1-webapi-kai-fa-wen-dang)
> **数据面**：[XTLS/Xray-core](https://github.com/XTLS/Xray-core)

---

## 目录

1. [一页结论](#1-一页结论)
2. [背景、角色与范围](#2-背景角色与范围)
3. [协议与 Xray 能力调研](#3-协议与-xray-能力调研)
4. [总体架构](#4-总体架构)
5. [关键设计决策](#5-关键设计决策)
6. [配置、数据模型与持久化](#6-配置数据模型与持久化)
7. [模块设计](#7-模块设计)
8. [soga-v1 接口实现规范](#8-soga-v1-接口实现规范)
9. [核心流程与状态机](#9-核心流程与状态机)
10. [限额、审计与流量归属](#10-限额审计与流量归属)
11. [安全设计](#11-安全设计)
12. [可观测性](#12-可观测性)
13. [性能与容量](#13-性能与容量)
14. [测试计划](#14-测试计划)
15. [里程碑与任务拆解](#15-里程碑与任务拆解)
16. [风险登记册](#16-风险登记册)
17. [待确认问题](#17-待确认问题)
18. [附录](#18-附录)

---

## 1. 一页结论

**要做什么**：一个 Go 单二进制 **节点侧后端**（soga 的替代品），以 **Xray-core 作为数据面**，以 **soga-v1 WebAPI 作为控制面协议**，对接已经实现该接口的面板。VLESS 为第一优先级节点类型。

**不做什么**：不实现面板；不复制 soga 代码；MVP 不维护 Xray 私有 fork；不覆盖 VMess/Trojan/SS 等其他节点类型。

**推荐技术路线**：

| 项目 | 决策 |
|---|---|
| 形态 | 单进程、单二进制，每进程一个 `NODE-ID` |
| Xray 集成 | **以 Go 库方式内嵌**（非子进程），固定版本 `v26.11.9` |
| 配置生成 | 面板数据 → 内部 `NodeSpec` → Xray JSON → `core.Config`，启动前可 dump/校验 |
| 用户同步 | 直接调用 Xray `proxy.UserManager` 增量 Add/Remove，不重启 |
| 流量统计 | Xray per-user stats counter，**读后原子清零** + 本地持久化 journal |
| 在线 IP | Xray `OnlineMap`（`policy.statsUserOnline`） |
| 限速/设备数/TCP 数 | 自研 **Gatekeeper outbound 包装器**（不 fork Xray） |
| 审计 | Gatekeeper 内完成规则匹配、阻断与上报（依赖 sniffing） |
| 节点配置变更 | 优先 **in-place inbound 热切换**；失败回滚；必要时整实例重启 |
| 可靠性 | 面板不可达时保持上次可用配置；流量 journal 至少一次上报 |
| 已知硬限制 | 现代 Xray 不再支持 `h2/http`；`ws/grpc` 已弃用；`speed/device/tcp` 非 Xray 原生；该站点不提供 ECH/decryption/xhttp_settings |
| 站点契约 | 已拿到 SSPanel-UIM + `SogaController` 源码：空用户列表合法、状态使用 `load`、审计规则为裸 regex/text、在线数来自 traffic 条数 |

**剩余风险**：审计规则裸 regex 在 soga 侧的解释、`status.load` 口径、在线数口径、部署版本与源码是否一致；见 §3.9 / §17。

**M0 状态**：核心风险已验证通过，证据见 **`M0-REPORT.md`**。

---

## 2. 背景、角色与范围

### 2.1 需求原文

> 基于 Xray-core 的节点后端服务程序，采用 soga-v1 WebAPI 接口，主要节点类型为 VLESS。

### 2.2 角色判定（重要）

soga 是**节点侧机场后端**：它运行在节点服务器上，周期性向**面板**拉取节点参数与用户列表，向面板上报流量、在线 IP、审计日志与节点状态。soga-v1 WebAPI 是**面板侧需要实现的服务端接口**。

因此本程序扮演 **soga 的替代者**：

```
┌─────────────────────────────┐         soga-v1 WebAPI          ┌──────────────────────────────┐
│  面板（已实现 soga-v1）       │  ◀── GET node/users/... ───────  │  linkc-node（本程序）          │
│  - 节点参数 / 用户 / 规则      │  ──── POST traffic/... ──────▶  │  - soga-v1 客户端              │
│  - 流量账本 / 在线状态         │                                 │  - 内嵌 Xray-core（VLESS）      │
└─────────────────────────────┘                                 └──────────────────────────────┘
```

- **本程序是 WebAPI 客户端**：发送 `API-KEY`、`NODE-ID`、`NODE-TYPE`，处理 `If-None-Match` / `304`。
- 已确认目标站点为 **SSPanel-UIM（glzjin 系）+ `SogaController`**：站点源码中的 `web/src/Controllers/Api/V1/SogaController.php` 就是 soga-v1 服务端实现，路由位于 `web/app/routes.php` 的 `/api/v1/*`。本程序是它的节点侧客户端。
- 开发期仍使用 **mock panel**（测试件）做回归；生产直接对接该站点。
- 若你的真实目标是「实现面板侧、让 soga 节点接进来」，请先确认；那种项目的核心是 HTTP 服务 + 数据库，不需要在面板里跑 Xray。见 [附录 D](#附录-d-若目标是面板侧)。

### 2.3 目标（Goals）

1. 完整实现 soga-v1 文档中 VLESS 节点所需的所有接口与请求头语义。
2. 以 Xray-core 运行 VLESS 节点：`tcp/ws/grpc/xhttp` × `none/tls/reality`，支持 `flow=xtls-rprx-vision`、`decryption`、ECH、REALITY 参数。
3. 用户热更新：新增/删除/变更 UUID 不重启进程、不中断其他用户。
4. 流量、在线 IP、节点状态按目标站点实际契约上报（见 §3.9）；面板故障时不丢数据、不中断服务。
5. 节点参数变化可安全应用：校验 → 备份 → 切换 → 失败回滚。
6. 可运维：单二进制、systemd/Docker、结构化日志、Prometheus 指标、本地健康检查。
7. 尽量支持 `speed_limit` / `device_limit` / `tcp_limit` 与审计规则，无法原生支持的能力要显式声明。

### 2.4 非目标（Non-Goals）

- 不实现订阅生成、计费、用户注册、工单、Web UI（属于面板）。
- 不实现 VMess/Trojan/SS/SSR/Hysteria/AnyTLS/Mieru 等节点类型（接口层预留扩展点）。
- 不做多节点集中编排（MVP 一进程一节点；后续可加 supervisor）。
- 不修改 Xray-core 源码；如未来必须 fork，单独走 ADR。
- 不做透明代理/TUN、DNS 过滤、流量伪装站点托管。
- MVP 不做自动 ACME 申请（先支持本地证书文件；ACME 列为 P2）。

### 2.5 假设

1. 目标站点（SSPanel-UIM + `SogaController`）已实现 soga-v1 的 8 个接口，且 HTTPS 可达。
2. 节点服务器为 Linux（amd64/arm64），允许绑定高位端口；443 需要 `CAP_NET_BIND_SERVICE`。
3. 面板下发的 `port/stream_type/tls_type/...` 是权威参数；本地只补充 Xray 必需但面板未提供的信息（证书、监听地址、ALPN 覆盖等）。
4. `speed_limit` 单位为 Mbps（十进制），`0` 表示不限。
5. `traffic` 的 `u/d` 是**自上次上报以来的增量字节数**（站点源码确认：累加 + 倍率）。
6. `alive_ip` 的 `ips` 是该用户的**当前在线 IP 集合**（站点按 60s 窗口 upsert，不清除离线 IP）。
7. 节点不进行用户身份二次鉴权；VLESS UUID 即身份。
8. 站点 `status` 使用 `load` 第一 token ×100 显示百分比；因此 `load` 必须是 CPU 小数（0..1）。

### 2.6 功能优先级（MoSCoW）

| 优先级 | 功能 |
|---|---|
| **Must** | `/node`、`/users` 拉取与 ETag；VLESS TCP/WS + TLS/REALITY；用户热更新；`/traffic`、`/status`（`load` 兼容）；空 users 合法；配置校验与回滚；last-known-good；systemd 部署 |
| **Should** | `/alive_ip`、`/audit_rules`、`/white_list`；审计 report/block；站点裸 regex 解析；XHTTP/GRPC；流量 journal 与崩溃恢复；Prometheus/healthz；入站热切换 |
| **Could** | ECH；用户/节点限速；设备数/TCP 限制；证书 ACME；geosite/geoip 自动更新；多节点 supervisor；`traffic.include_online_zero` |
| **Won't（本期）** | 面板/UI、订阅、计费、其他节点类型、多租户、私有 Xray fork |

---

## 3. 协议与 Xray 能力调研

### 3.1 soga-v1 接口清单

| # | 方法 | 路径（相对 `webapi_url`） | 方向 | 用途 | ETag | 建议周期 |
|---|---|---|---|---|---|---|
| 1 | GET | `/api/v1/node` | 本程序拉取 | 节点参数 + basic | 支持 | `pull_interval` |
| 2 | GET | `/api/v1/users` | 本程序拉取 | 用户列表 | 支持 | `pull_interval` |
| 3 | GET | `/api/v1/audit_rules` | 本程序拉取 | 审计规则 | 支持 | `pull_interval`（可独立） |
| 4 | GET | `/api/v1/white_list` | 本程序拉取 | 审计白名单 | 支持 | `pull_interval`（可独立） |
| 5 | POST | `/api/v1/traffic` | 本程序推送 | 用户流量增量 | — | `push_interval` |
| 6 | POST | `/api/v1/alive_ip` | 本程序推送 | 在线 IP | — | `push_interval` |
| 7 | POST | `/api/v1/audit_log` | 本程序推送 | 审计命中 | — | 事件驱动/合并 |
| 8 | POST | `/api/v1/status` | 本程序推送 | CPU/内存/磁盘/uptime | — | `push_interval` |

**请求头**（所有请求）：

| Header | 值 | 说明 |
|---|---|---|
| `API-KEY` | 面板分配的 key | 不要写日志 |
| `NODE-ID` | 节点 ID | 与本地配置一致 |
| `NODE-TYPE` | `vless` | MVP 固定 |
| `IF-NONE-MATCH` | 上次响应的 ETag | 仅 GET；无缓存时不带 |
| `Content-Type` | `application/json` | POST |
| `Accept` | `application/json` | 全部 |

**响应约定**：

- GET：`200` + JSON；或 `304 Not Modified`（无 body，沿用缓存）。
- POST：HTTP 200 + `{"code":0,"message":""}` 表示成功；`code != 0` 视为业务失败。
- 文档未定义 4xx/5xx 的具体语义，需按保守策略处理（见 §8.4）。

### 3.2 VLESS 节点参数（`/api/v1/node`）

```jsonc
{
  "basic": {                       // 可省略字段均有默认值
    "pull_interval": 60,           // 拉取周期，秒
    "push_interval": 60,           // 推送周期，秒
    "speed_limit": 0               // 节点总限速，Mbps；0=不限
  },
  "config": {
    "port": 12345,
    "stream_type": "tcp",          // tcp | ws | http | h2 | grpc | xhttp
    "tls_type": "reality",         // none | tls | reality
    "ech_key": "",                 // v2.17.1+；TLS 时生效
    "flow": "xtls-rprx-vision",    // reality 区块中；vision
    "server_names": ["www.example.com"],
    "private_key": "",             // REALITY 私钥（base64url，32B）
    "short_ids": ["0123456789abcdef"],
    "dest": "www.example.com:443", // REALITY 回落目标
    "decryption": "",              // v2.13.7+；空/none 表示不启用 VLESS 加密
    "path": "/path",               // ws/xhttp
    "service_name": "service",     // grpc
    "xhttp_settings": {            // xhttp
      "host": "example.com",
      "path": "/yourpath",
      "mode": "auto",
      "extra": {
        "headers": {},
        "xPaddingBytes": "100-1000",
        "noSSEHeader": false,
        "scMaxEachPostBytes": 1000000,
        "scMaxBufferedPosts": 30,
        "scStreamUpServerSecs": "20-80"
      }
    }
  }
}
```

> 文档中 `flow/server_names/private_key/short_ids/dest` 标注在 “reality 配置” 下，但 `flow` 对 TLS 同样有意义。实现时按“节点级 flow，对所有用户生效”处理，并做合法性校验。
>
> **目标站点实际**：只下发上述字段的子集，不含 `ech_key`/`decryption`/`xhttp_settings`/`host`/`alpn`；`tls_type` 的 `xtls` 会被映射成 `none`。详见 §3.9.2。

### 3.3 用户（`/api/v1/users`，VLESS）

```jsonc
[
  {
    "id": 1,
    "uuid": "701534e1-8540-49f3-87af-71680a1b97ff",
    "speed_limit": 0,     // Mbps，0=不限
    "device_limit": 0,    // 在线 IP 数限制，0=不限
    "tcp_limit": 0        // TCP 连接数限制，0=不限
  }
]
```

> **目标站点实际**：每项还会返回 `method/protocol/protocol_param/obfs/obfs_param`；`tcp_limit` 恒为 0；`device_limit` 来自 `node_connector`；超流量用户默认被跳过（`keep_connect=true` 时保留并限速 1 Mbps）；节点超带宽时返回 `[]`。**空数组是合法结果**。详见 §3.9.3。

### 3.4 审计规则 / 白名单

```jsonc
// GET /api/v1/audit_rules
[
  { "id": 1, "rule": "google" },
  { "id": 2, "rule": "regexp:.*google\\.com" }
]

// GET /api/v1/white_list（并与本地 whiteList 文件合并）
[
  "google",
  "regexp:.*google\\.com",
  "domain:google.com",
  "full:google.com",
  "geosite:google",
  "ip:192.168.0.0/16",
  "geoip:cn",
  "port:1-1024,12345,23456"
]
```

规则语法文档只给了示例，完整语义需要确认。计划支持的语法见 §10.4，未知前缀默认“跳过并告警”（fail-open），可配置为严格模式。

> **目标站点实际**：`SogaController` 返回 `detect_list` 全表，`rule = regex ?: text`，**裸值、不加 `regexp:` 前缀**；白名单仅来自环境变量 `soga_audit_white_list`（逗号分隔）。因此规则解析必须支持“裸 regex”模式，见 §10.4。

### 3.5 上报结构

```jsonc
// POST /traffic
[{ "id": 1, "u": 1024, "d": 2048 }]

// POST /alive_ip
[{ "id": 1, "ips": ["1.2.3.4", "2.3.4.5"] }]

// POST /audit_log
[{ "user_id": 1, "audit_id": 2 }]

// POST /status
{
  "cpu": 23.5,
  "mem":   { "total": 1073741824, "used": 536870912 },
  "swap":  { "total": 1073741824, "used": 536870912 },
  "disk":  { "total": 1073741824, "used": 536870912 },
  "uptime": 123456
}
```

> **目标站点实际**：`SogaController::status()` 优先读取 `load`（字符串/数字），只持久化 `load`（varchar 32）与 `uptime`，忽略 `swap/disk/结构化 mem`；前端把 `explode(' ', load)[0] * 100` 显示为百分比。因此必须发送 `load = CPU 小数（0..1）`，例如 `"load":"0.235"`；详见 §3.9.8。

### 3.6 Xray-core 能力与缺口

| 需求 | Xray 现成能力 | 结论 |
|---|---|---|
| VLESS 服务端 | `proxy/vless/inbound` | 原生支持 |
| TCP/WS/GRPC/XHTTP | `transport/internet/{tcp,websocket,grpc,splithttp}` | 支持；WS/GRPC 已弃用但仍可用 |
| TLS | `transport/internet/tls` | 支持；证书文件热加载 |
| REALITY | `transport/internet/reality` | 支持；仅 RAW/XHTTP/GRPC |
| ECH 服务端 | `tlsSettings.echServerKeys`（JSON ≥ v26.9.30） | 支持；需确认格式转换 |
| 用户热更新 | `proxy.UserManager.AddUser/RemoveUser` | 支持，无需重启 |
| 每用户流量 | `user>>>{email}>>>traffic>>>uplink/downlink` | 支持；需开启 policy stats |
| 在线 IP | `policy.statsUserOnline` + `OnlineMap` | 支持 |
| 用户限速 | **无** | 需 Gatekeeper 自研 |
| 设备数限制 | **无** | 需 Gatekeeper 自研 |
| TCP 连接数限制 | **无** | 需 Gatekeeper 自研 |
| 审计命中上报 | **无原生用户级上报** | 需 Gatekeeper 自研（或路由 webhook） |
| 节点 CPU/内存/磁盘 | **无** | 需 OS 采集 |
| h2/http 传输 | **已移除**（v24.12.18+） | 需映射或拒绝 |
| 零拷贝 splice | 原生（Vision） | 限速时必须关闭 |

> **目标站点能力边界**：站点当前不下发 `ech_key`、`decryption`、`xhttp_settings`；Xray 侧仍保留这些能力作为可选扩展，MVP 不依赖它们，也不会因缺失而拒绝节点。

### 3.7 版本兼容矩阵（调研结论）

| 能力 | 版本事实 | 本方案 |
|---|---|---|
| soga-v1 协议 | soga v2.12.3+ 支持；`decryption` v2.13.7+；ECH v2.17.1+ | 按文档实现；版本特性可探测/告警 |
| `go.mod` 版本要求 | v25.9.11 → Go 1.25；v26.6.1 → 1.26；v26.11.9 → 1.27 | 固定 Go 1.27+ |
| `h2/http` 传输 | v24.11.30 弃用警告；**v24.12.18 起移除** | 拒绝或映射 XHTTP（默认拒绝） |
| `ws/grpc` | 新版本仅打印弃用警告 | 保留支持，日志提示 |
| ECH 服务端 JSON | `echServerKeys` 在 v26.9.30+ 的 `infra/conf` 可见 | 固定 v26.11.9 |
| `GetUsersStats` | v26.6.1+ 才有 | 不使用（直接访问内部 feature） |
| `statsUserOnline` | v25.3.6+ 已存在 | 使用 |
| `decryption: none` | 新版本强校验 | 空值归一化为 `none` |
| REALITY 传输 | 当前仅 RAW/XHTTP/GRPC | 校验组合 |
| XHTTP `extra`/`xmux` | v24.12+ 逐步完善 | 固定新版本 |

**固定版本建议**：GitHub release `v26.11.9`（当前最新稳定版），Go 1.27。

> **M0 实测**：`v26.11.9` 不是合法 Go module semver，`go get github.com/xtls/xray-core@v26.11.9` 会失败。必须固定该 release 的 commit 伪版本：
>
> `github.com/xtls/xray-core v1.260327.1-0.20261109052709-676eaf66f8f0`（commit `676eaf66f8f05d4d558ee7a74b4736623d3feeb9`）。
>
> `go.mod` 中记录 release tag 作为注释，升级时同时更新 commit 与 release 记录。升级策略不变：每次升级前跑完整兼容矩阵测试。

### 3.8 关键源码依据

- 用户流量/在线 IP：`app/dispatcher/default.go`（`getLink`、`trackOnlineIP`）
- 在线 IP 映射：`app/stats/online_map.go`
- Stats API：`app/stats/command/command.proto`
- 动态用户/入站 API：`app/proxyman/command/command.proto`
- VLESS 入站与用户：`proxy/vless/inbound/inbound.go`、`proxy/vless/validator.go`
- VLESS 配置：`infra/conf/vless.go`
- 传输移除/弃用：`infra/conf/transport_internet.go`
- REALITY 配置：`infra/conf/transport_security.go`
- XHTTP 配置：`infra/conf/transport_method.go`
- 用户流量/在线策略：`infra/conf/policy.go`
- splice 判定：`proxy/proxy.go`（`CopyRawConnIfExist`）
- 入站监听 `SO_REUSEPORT`：`transport/internet/system_listener.go`
- 运行时加入入站：`core/xray.go`（`AddInboundHandler`）
- **目标站点**：`web/src/Controllers/Api/V1/SogaController.php`、`web/app/routes.php`、`web/src/Models/{Node,User,TrafficLog,DetectRule,DetectLog,NodeOnlineLog}.php`、`web/src/Utils/{URL,Tools}.php`、`web/resources/views/metron/admin/node/{create,edit}.tpl`

### 3.9 目标站点实测契约（SSPanel-UIM + SogaController）

> **依据**：目标站点源码 `web/src/Controllers/Api/V1/SogaController.php`、`web/app/routes.php`、`web/src/Models/*.php`、`web/src/Utils/{URL,Tools}.php`（归档时间 2026-11-13）。以下为源码实际行为，**优先于公开文档**；上线前需确认部署版本与归档源码一致。

#### 3.9.1 鉴权与节点识别

- `API-KEY` 从请求头读取；也兼容 query `?key=`。校验 `Config::getMuKey()`（`muKey` + `muKeyList`）。
- `WebAPI=false` → HTTP 503；`checkNodeIp=true` 时：
  - 非 loopback 的源 IP 必须出现在某节点 `node_ip` 中；
  - 若声明了 `NODE-ID`，该节点 `node_ip` 必须包含源 IP，否则 403。
- `NODE-ID` 从请求头或 `?node_id=` 读取；为空/0 时按源 IP 反查节点。
- `NODE-TYPE` 大小写不敏感；`vless` 正常映射。
- 通过 guard 且找到节点后，各 endpoint 会 `touchNode()`（更新 `node_heartbeat`）；节点在线阈值 300 秒。guard 失败或节点不存在时不会更新心跳。

#### 3.9.2 `GET /api/v1/node`

- 响应 = `basic` + `config` + **`node_type`** + **`traffic_rate`**。`traffic_rate` 可忽略；`node_type` 用于校验（若不是 `vless`，说明请求头与节点 `sort` 配置不一致，应拒绝应用并告警）。
- `basic.pull_interval` / `push_interval` 来自 `node.server` 字符串参数（如 `pull_interval=60|push_interval=60`），默认 60；`speed_limit` 来自 `node_speedlimit`。
- VLESS `config` 实际字段：
  - `port`、`stream_type`、`tls_type`（`none|tls|reality`）、`flow`、`server_names`、`private_key`、`short_ids`、`dest`；
  - 可选 `xver`、`path`（非空时）、`service_name`（非空时）。
- **站点不提供**：`ech_key`、`decryption`、`xhttp_settings`、`host`、`alpn`。
- 若站点配置 `net=xhttp`，只会带 `path`；本代理按 `xhttpSettings.mode=auto`、`host` 留空处理。需要 `packet-up/stream-up/stream-one` 或 `extra` 时，只能本地扩展，站点当前不支持。
- `tls_type` 映射：`reality` → `reality`；`tls` → `tls`；其他（包括 `xtls`）→ `none`。若管理员把 `xtls` 当作 TLS，节点会收到 `none` + 可能的 vision flow，属于站点配置问题，应在应用前拒绝并给出明确告警。
- `dest` 为空且存在 `server_names` 时，默认 `<server_names[0]>:443`。
- VLESS `node.server` 示例（来自站点后台模板）：
  `example.com;443;0;tcp;reality;sni=www.microsoft.com|flow=xtls-rprx-vision|fp=chrome|pbk=PUBLIC_KEY|sid=SHORT_ID|spx=/|private_key=PRIVATE_KEY|short_ids=SHORT_ID|server_names=www.microsoft.com|dest=www.microsoft.com:443`
- ETag：`"` + `sha1(json)` + `"`；`IF-NONE-MATCH` 精确匹配时返回 304（带 ETag、无 body）。

#### 3.9.3 `GET /api/v1/users`

- 过滤：`class >= node_class`、`node_group`、`enable=1`、`expire_in > NOW()`、`is_multi_user`；`is_admin=1` 始终包含。
- **超流量用户**：`transfer_enable <= u + d` 时默认跳过；`keep_connect=true` 时保留并强制 `speed_limit=1`。
- **节点超带宽限制**（`node_bandwidth_limit != 0 && node_bandwidth > limit`）→ 返回 `[]`。
- 返回项在文档字段之外还包含 `method/protocol/protocol_param/obfs/obfs_param`；`tcp_limit` **恒为 0**；`device_limit` 来自 `node_connector`；`speed_limit` 为 float。
- `uuid` 来自 `User::getUuid()`：优先 DB `uuid` 字段，否则由 `uuid3(id|passwd)` 生成。
- **空数组是合法业务结果**：可能因为全员超流量、节点超带宽、筛选后无用户。实现必须允许应用空列表（见 §7.4 的安全阀调整）。

#### 3.9.4 `GET /api/v1/audit_rules` / `GET /api/v1/white_list`

- 审计规则来自 `detect_list` 全表：`{id, rule}`，其中 `rule = regex ?: text`，**原样返回，不保证 `regexp:` 等前缀**；`type` 字段不返回。
- 白名单仅来自环境变量 `soga_audit_white_list`（逗号分隔）；站点没有独立白名单表。节点本地 whiteList 文件仍需合并。

#### 3.9.5 `POST /api/v1/traffic`

- 接受数组或单对象；用户键支持 `id` 或 `user_id`。
- `u/d` 先 `(float)` 再 `round()`，负数归零；用户表累加 `u/d` 时乘以 `node.traffic_rate`；同时更新 `user.t`。
- 每个用户写一条 `user_traffic_log`；节点 `node_bandwidth` 累加**原始** `u+d`（不含倍率）；`node_heartbeat` 更新。
- 写一条 `ss_node_online_log`，`online_user = count(payload)`。**在线用户数来自 traffic 条数，不由 alive_ip 更新**。
- 无幂等键；重试会重复累加用户与节点流量。

#### 3.9.6 `POST /api/v1/alive_ip`

- 接受 `ips` 数组或单个 `ip`；去掉 `::ffff:`；`FILTER_VALIDATE_IP` 校验；同一请求内去重。
- `alive_ip` 表：同 user+node+ip 且 60 秒内已存在则更新 `datetime`，否则插入新行。
- **不会清除离线 IP**；节点不发送某用户也不会删除其历史记录。
- 不写 `ss_node_online_log`。

#### 3.9.7 `POST /api/v1/audit_log`

- 用户键 `user_id`/`id`；规则键 `audit_id`/`list_id`/`rule_id`。
- 去重：同 `(user_id, list_id, node_id)` 且 **同一自然日** 只写一条 `detect_log`；跨日可再次记录。

#### 3.9.8 `POST /api/v1/status`

- 读取 `load`（优先）或 `cpu`；若 `load`/`cpu` 是数组/对象，则取其中的 `cpu` 和 `mem` 拼成 `"cpu/mem"`。
- 只持久化 `ss_node_info.load`（varchar 32）和 `uptime`；**忽略 `swap`、`disk`、结构化 `mem`**。
- 站点前端计算：`latest_load = explode(' ', load)[0] * 100`，显示为百分比。因此 **`load` 的第一个 token 必须是 0..1 的 CPU 使用率小数**（如 `0.235`），否则会把 `23.5` 显示成 `2350%`。
- 推荐同时发送两套字段，兼容本站与文档型面板：
  ```json
  {
    "load": "0.235",
    "cpu": 23.5,
    "mem": {"total": 1073741824, "used": 536870912},
    "swap": {"total": 0, "used": 0},
    "disk": {"total": 1073741824, "used": 536870912},
    "uptime": 123456
  }
  ```
  本站取 `load`；文档型面板取 `cpu/mem/...`。

#### 3.9.9 错误码与响应

| 场景 | HTTP | body |
|---|---|---|
| 成功（POST） | 200 | `{"code":0,"message":""}` |
| 业务失败（traffic 事务异常） | 200 | `{"code":1,"message":"update failed"}` |
| API key 缺失/错误 | 401 | `{"code":401,"message":"..."}` |
| IP/节点不匹配 | 403 | `{"code":403,"message":"..."}` |
| WebAPI 关闭 | 503 | `{"code":503,"message":"..."}` |
| 节点不存在 | 404 | `{"code":404,"message":"node not found"}` |
| GET 304 | 304 | 空 body + ETag |

#### 3.9.10 对实现的直接影响（站点 profile）

新增 `panel.profile: sspanel-uim-soga`（默认自动识别/显式指定），应用以下默认值：

| 配置 | 站点默认 | 原因 |
|---|---|---|
| `users.allow_empty` | `true` | 全员超流量/节点超带宽时合法返回 `[]` |
| `users.mass_remove_guard` | `warn`（非拒绝） | 过期/超流量导致大量用户消失是正常业务 |
| `status.mode` | `load_fraction` | 本站按第一 token ×100 显示 |
| `audit.rule_mode` | `auto` | 站点返回裸 regex 或 text |
| `audit.dedupe_ttl` | `24h`（自然日） | 站点按自然日去重 |
| `alive_ip.clear_offline` | `false` | 站点不清除离线 IP |
| `traffic.include_online_zero` | `false` | 避免写 0/0 流量日志；如需要准确在线数可开启 |
| `ech` / `decryption` / `xhttp_settings` | 缺省忽略 | 站点不下发这些字段 |
| `compat.xtls_mode` | `reject-with-hint` | 站点把 `xtls` 映射为 `none`，vision 组合会失败 |

其余行为（超流量跳过、`keep_connect` 限速 1、`traffic_rate` 由面板乘、`device_limit=node_connector`、`tcp_limit=0`）直接遵循站点。

---

## 4. 总体架构

### 4.1 组件图

```
                         ┌───────────────────────────────┐
                         │  面板（soga-v1 WebAPI 服务端）  │
                         └───────────────▲───────────────┘
                                         │ HTTPS + ETag
┌────────────────────────────────────────┴─────────────────────────────────────┐
│ linkc-node（单进程）                                                          │
│                                                                              │
│  ┌──────────────┐   ┌──────────────┐   ┌──────────────┐   ┌───────────────┐  │
│  │  Scheduler   │──▶│ Panel Client │──▶│  Reconciler  │──▶│ Xray Runtime  │  │
│  │  pull/push   │   │ ETag/retry   │   │ diff/apply   │   │  in-process   │  │
│  └──────────────┘   └──────────────┘   └──────┬───────┘   └───────┬───────┘  │
│         │                                      │                   │          │
│         │              ┌───────────────────────▼───────────────────▼───────┐  │
│         │              │ Xray-core 实例                                     │  │
│         │              │  VLESS inbound ──▶ dispatcher ──▶ Gatekeeper ──▶    │  │
│         │              │                                 Freedom(dial)      │  │
│         │              │  Stats Manager / Online Maps / User Manager        │  │
│         │              └────────────────────────────────────────────────────┘  │
│         │                                                                      │
│  ┌──────▼───────┐  ┌──────────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │ Traffic      │  │ Audit Engine │  │ Status       │  │ Admin/Metrics      │  │
│  │ collector    │  │ matcher/dedupe│ │ collector    │  │ unix socket        │  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘  └───────────────────┘  │
│         │                 │                 │                                │
│  ┌──────▼─────────────────▼─────────────────▼─────────────────────────────┐  │
│  │ bbolt：ETag 缓存 / last-known-good / 流量 journal / 审计去重 / 状态     │  │
│  └────────────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 4.2 进程模型

- **默认：一进程一节点**（与 soga 一致）。`NODE-ID` 在本地配置中固定；通过 systemd template 运行多实例。
- 稳态每进程只有一个 Xray `core.Instance`；节点配置变化时优先热切换 inbound，不重启进程。进程级重建时先停旧再启新（MVP 不做蓝绿双实例）。
- 本地管理面只暴露 **Unix domain socket**（权限 0600），不开 TCP 管理端口。
- 数据面监听地址默认 `0.0.0.0` / `::`，可配置。

### 4.3 线程/协程模型

| 协程 | 职责 | 频率 |
|---|---|---|
| `pullLoop` | 拉取 node/users/audit/whitelist；触发 reconcile | `pull_interval`（默认 60s） |
| `pushLoop` | 推送 traffic/alive_ip/status | `push_interval`（默认 60s） |
| `collectLoop` | 读取并清零 Xray 流量 counter，写 journal | 独立间隔（默认 30s，≤ push） |
| `auditCollector` | 接收 Gatekeeper 审计事件、去重、批量上报 | 事件 + 1s debounce |
| `coreSupervisor` | 监控 Xray 实例健康，异常时按 last-known-good 重建 | 5s |
| `statusSampler` | 采样 CPU/mem/swap/disk（供 push 使用） | 5s |
| `deviceGuard` | 设备数/连接数统计与告警（enforce 在 Gatekeeper 内） | 5s |

所有循环共享一个 `context.Context`，`SIGTERM` 时按顺序取消。

### 4.4 数据流

**控制面下行**：

```
panel /node ──▶ NodeSpec ──▶ 校验 ──▶ 生成 Xray 配置 ──▶ 构建/切换 inbound
panel /users ─▶ UserSpec[] ─▶ diff ──▶ AddUser/RemoveUser ──▶ 更新 limit/audit 表
panel /audit_rules ─▶ 审计 matcher ──▶ atomic swap（无需重启）
panel /white_list ──▶ 合并本地文件 ──▶ 审计 matcher
```

**数据面上行**：

```
Xray stats counters ──▶ collectLoop（读+清零）──▶ journal ──▶ pushLoop ──▶ POST /traffic
Xray OnlineMap ──────▶ pushLoop ──▶ 聚合/去重 ──▶ POST /alive_ip
Gatekeeper 审计事件 ──▶ auditCollector（去重）──▶ POST /audit_log
/proc + statfs ──────▶ statusSampler ──▶ POST /status
```

---

## 5. 关键设计决策

> 每条决策都包含备选方案与理由；未决项在 [§17](#17-待确认问题) 中列出。

### D1. 语言与运行时：Go + 单二进制

- **选择**：Go 1.27，`CGO_ENABLED=0`，静态二进制。
- **理由**：Xray-core 是 Go 库，内嵌必须同语言；部署简单；性能足够。
- **备选**：Rust/C++ 只能走子进程 + gRPC，后续可选。

### D2. Xray 集成：库内嵌，而非子进程

| 维度 | 内嵌库 | 子进程 + gRPC |
|---|---|---|
| 动态用户/统计 | 直接 feature 访问，零 IPC | 需要 API 服务与 socket |
| 限速/审计网关 | 可注入自定义 outbound | 不可注入，需外部实现 |
| 升级 | 重新编译 | 替换二进制 |
| 故障隔离 | 进程内 panic 影响 agent | 子进程崩溃可重启 |
| 部署 | 单文件 | 两个文件 + 配置 |

- **选择**：内嵌。理由：限速/审计/设备数需要连接级 hook，只有内嵌能优雅实现。
- **依赖注册**：只 import 实际需要的 Xray 组件（dispatcher、proxyman、stats、policy、log、router、freedom、blackhole、vless、各 transport、geodata、JSON loader），不要直接 import `main/distro/all`，避免把 Xray 的命令行/命令注册进 agent。
- **缓解**：顶层 goroutine 加 recover；systemd 自动重启；状态 journal 保证重启后流量不丢（at-least-once 与重复计入风险见 §7.5 / §10.6）。

### D3. 配置生成：JSON 优先，构建后注入自定义 outbound

- **流程**：`NodeSpec` → `conf.Config`（Go 结构）→ JSON（可 dump/日志/测试）→ `serial.DecodeJSONConfig` → `Build()` → 后处理 `core.Config`：
  - 找到 tag=`direct` 的 freedom outbound；
  - 将其 `ProxySettings` 替换为自定义 `GatekeeperConfig`（内嵌原始 `freedom.Config`）。
- **理由**：JSON 便于 golden 测试和人工排障；自定义 outbound 无法通过 JSON loader 注册，而后处理只需替换一个 typed message。
- **备选**：完全 proto 构建（类型安全但冗长）；fork Xray（不推荐）。

### D4. 不做 Xray fork

- **选择**：不修改 Xray 源码，不维护 patch。
- **理由**：安全更新、版本升级成本最低；Gatekeeper 已能覆盖核心缺口。
- **代价**：无法获得真正的零拷贝限速；设备/TCP 限制只能在认证后执行（见 §10）。

### D5. 用户同步：直接调用 `proxy.UserManager`

- 通过 `inbound.Manager.GetHandler(tag)` → `proxy.GetInbound()` → `proxy.UserManager` 直接 Add/Remove。
- 不启用 commander/gRPC；如未来需要远程调试，可单独开 Unix socket API。
- 用户 email 采用稳定映射：`u<id>`（小写），用于 stats/online map 反查。
- **注意**：Xray 的 VLESS validator 会对 UUID 做 `ProcessUUID`（把第 6、7 字节清零，即 UUID 的 `time_hi_version` 字段）后再索引；两个 UUID 若仅这两个字节不同会冲突，需在同步时检测并拒绝。

### D6. 流量：读后原子清零 + 持久化 journal

- 每 `collect_interval`：对每个用户两次 `GetCounter`，`Set(0)` 返回旧值 → 写入 bbolt journal（fsync）→ 清空内存。
- 推送成功后才标记 journal 已确认；失败保留重试。
- **语义**：至少一次（at-least-once）；崩溃窗口内可能重复计入，需文档说明并尽量缩小窗口。
- **备选**：不 reset、维护绝对快照；重启后需要基线，容易双计/漏计，作为可选模式。

### D7. 在线 IP：使用 Xray OnlineMap

- 配置 `policy.levels."0".statsUserOnline=true`，dispatcher 自动维护 `user>>>u<id>>>online`。
- 通过 `stats.Manager.GetOnlineMap` 遍历，得到 IP（refcount 在连接结束时移除）。
- 设备数限制使用同一份数据 + Gatekeeper 自己的连接表做双校验。

### D8. 限速/设备/TCP：Gatekeeper outbound

- 自定义 `proxy.Outbound`，包住 freedom：
  - 入口读取 `session.InboundFromContext(ctx).User.Email` 与 Source IP；
  - 执行设备数/TCP 限制判断；
  - 执行审计规则匹配；
  - 对 `link.Reader`/`link.Writer` 加 token bucket（仅当用户或节点限速非 0）；
  - 委托内部 freedom `Process` 完成拨号与转发。
- **关键细节**：限速用户必须禁止 splice（`inbound.CanSpliceCopy=3`），否则 Vision 会绕过 link 包装器直连，导致限速失效。
- **M0 已验证**：在 REALITY + Vision 下，设置 `CanSpliceCopy=3` 后 4 Mbps 限速 E2E 通过（512 KiB ≈ 0.72s）；不设置时 splice 会绕过限速。
- **备选**：fork Xray 在 dispatcher 内限速（更强但维护成本高）。

### D9. 审计：Gatekeeper 内匹配并阻断

- 不依赖 Xray 路由规则/webhook，避免版本绑定与异步丢失。
- 开启 sniffing（`http/tls/quic`）获取域名；规则匹配顺序：白名单 → 审计规则。
- 目标站点返回裸 regex/text（无 `regexp:` 前缀）：解析器默认 `rule_mode=auto`，含正则元字符按 regex、否则 keyword；详见 §10.4。
- 命中后：按配置阻断（关闭/黑洞）或仅上报；事件进入本地队列，按 `(user, rule)` 去重后上报。
- 规则热更新：重新编译 matcher + `atomic.Value` 替换，不重启。

### D10. 节点配置变更：in-place inbound 切换

- 只变更用户/限额/审计 → 完全热更新，无重启。
- 只变更入站参数（port/transport/tls/reality/flow/decryption）→ 新建 inbound（新 tag），加入实例，等待监听成功，再移除旧 tag；若同端口依赖 `SO_REUSEPORT`（Xray 默认设置），可重叠；失败则回滚旧配置。
- 变更出站/路由/策略等全局配置（MVP 不支持面板控制）→ 整实例重建（先停旧再启新，秒级断线；蓝绿双实例作为 P2）。
- **理由**：保留 stats 计数器和在线状态，减少断线。

### D11. 持久化：bbolt 单文件

- 状态量小（几 MB），bbolt 提供事务、原子提交、无 CGO。
- 备选 SQLite（modernc）— 依赖更大；JSON 文件 — 崩溃一致性差。
- 所有缓存/密钥文件权限 0600，目录 0700。

### D12. 一进程一节点

- 与 soga 的 `NODE-ID` 模型一致；隔离故障与资源。
- 多节点部署用 systemd template / 多容器。
- 未来如需「一个进程多节点」，`Reconciler` 与 `Xray Runtime` 已按 node 维度抽象，可扩展。

---

## 6. 配置、数据模型与持久化

### 6.1 本地配置（YAML 草案）

```yaml
# /etc/linkc-node/config.yaml
panel:
  base_url: "https://panel.example.com/custom/uri/"   # 必须与面板授权/绑定路径一致
  profile: "sspanel-uim-soga"                          # 站点 profile；见 §3.9.10
  api_key: "${PANEL_API_KEY}"                          # 支持环境变量展开
  node_id: "12"
  node_type: "vless"
  timeout: 15s
  tls:
    verify: true
    ca_file: ""            # 自定义 CA
    server_name: ""        # SNI 覆盖
    client_cert_file: ""   # 可选 mTLS
    client_key_file: ""
  allow_http: false        # 生产禁止
  follow_redirects: false  # 避免路径绑定失效
  proxy: ""                # 可选 HTTP/SOCKS 代理

node:
  listen: "0.0.0.0"        # 与面板 port 组合
  tls:
    certificates:          # tls_type=tls 时必需；可多张按 SNI 选择
      - cert_file: "/etc/linkc/fullchain.pem"
        key_file: "/etc/linkc/privkey.pem"
    alpn: []               # 为空则按传输类型自动选择
    min_version: ""        # 空=自动（vision 强制 1.3，其余默认 1.2）
  reality:
    xver: 0
    mldsa65_seed: ""       # 可选，本地扩展
  sniffing:
    mode: "auto"           # auto = 有审计规则时开启
  dns:
    servers: []            # 为空使用系统 DNS
    strategy: "UseIPv4v6"
  limits:
    mode: "enforce"        # enforce | report | off
    device_limit_ipv6_prefix: 128  # 可选 64
    burst_bytes: 65536
  freedom:
    allow_private_targets: false   # 生产默认阻断私网；M0 e2e 临时开启
  audit:
    enabled: true
    action: "block"        # block | report
    rule_mode: "auto"      # auto | regex | keyword | prefixed（站点为裸 regex/text）
    whitelist_file: "/etc/linkc/whitelist.txt"
    dedupe_ttl: "24h"      # 站点按自然日去重
    resolve_domain_for_ip_rules: true
    strict_rules: false
  users:
    allow_empty: true      # 站点：全员超流量/节点超带宽合法返回 []
    mass_remove_guard: "warn"  # warn | reject
  traffic:
    include_online_zero: false # true 时用 0/0 条目上报在线数（会写 0 流量日志）
  alive_ip:
    clear_offline: false   # 站点不清除离线 IP，无需发送空列表
  status:
    mode: "load_fraction"  # load_fraction | doc
  geodata:
    dir: "/usr/share/xray" # geosite.dat / geoip.dat 所在目录
    auto_update: false     # MVP 不做自动下载

storage:
  path: "/var/lib/linkc-node"
  max_journal_bytes: 67108864

logging:
  level: "info"            # debug | info | warn | error
  format: "json"

admin:
  socket: "/run/linkc-node/admin.sock"
  metrics: true
  pprof: false

compat:
  h2_mode: "reject"        # reject | xhttp
  xtls_mode: "reject-with-hint" # 站点把 xtls 映射为 none；vision 组合会失败
```

### 6.2 内部领域模型

| 模型 | 字段 | 说明 |
|---|---|---|
| `NodeSpec` | id, type, basic{pull,push,speed}, config{port,stream,tls,...} | 面板 `/node` 的规范化 |
| `UserSpec` | id(int64), uuid, speedMBps, deviceLimit, tcpLimit | email=`u<id>` |
| `AuditRule` | id, raw, matcher, compiledAt | 规则编译结果 |
| `WhiteList` | raw[], matcher | 合并本地文件 |
| `AppliedNode` | tag, generation, hash, config dump | last-known-good |
| `TrafficBatch` | seq, userID, up, down, createdAt, acked | journal 条目 |
| `AuditEvent` | userID, ruleID, ts | 去重后入队 |
| `AliveSnapshot` | userID, ips[], seenAt | 在线 IP 快照 |

### 6.3 bbolt bucket 设计

| Bucket | Key | Value | 说明 |
|---|---|---|---|
| `meta` | `schema_version` / `node_id` / `core_epoch` | string/int | schema 迁移、核心启动代号 |
| `etag` | endpoint | `{etag, body, fetched_at, sha256}` | GET 缓存；敏感字段随文件权限保护 |
| `last_good` | `node` / `users` / `audit_rules` / `white_list` | JSON + sha256 | 面板不可达时启动 |
| `traffic` | `seq`（8B BE） | `{user_id,u,d,created,acked}` | 待上报流量 |
| `traffic_meta` | `next_seq` / `last_push_at` | int64 | 序号与时间 |
| `audit_queue` | `seq` | `{user_id,audit_id,created}` | 待上报审计 |
| `audit_dedupe` | `userID:ruleID` | `last_reported_at` | 去重 TTL |
| `alive_last` | `userID` | `{hash,ips,updated_at}` | 变更检测 |
| `counters` | `userID` | `{up_total,down_total,last_collect_at}` | 调试/对账用（可选） |

**事务策略**：

- 采集：`counter.Set(0)` 与 journal 写入必须成对。由于清零不可逆，顺序必须是：
  1. `Set(0)` 取得增量；
  2. 立即写入 bbolt（`NoSync=false`）；
  3. 若写盘失败，增量保留在内存并阻止下一轮采集，告警；进程崩溃则丢失该窗口。
- 推送：按 seq 升序批量读取，成功后标记 `acked=true`（或删除并推进 `last_acked_seq`）；失败保留。
- ETag 缓存与 last_good 写入使用单独事务，互不阻塞。

### 6.4 文件与目录

```
/etc/linkc-node/config.yaml         # 配置（0600）
/etc/linkc/whitelist.txt            # 本地审计白名单
/var/lib/linkc-node/state.db        # bbolt（0600）
/var/lib/linkc-node/state.db.bak    # 升级前备份
/run/linkc-node/admin.sock          # 管理 socket（0600）
/var/log/linkc-node/                # 可选，默认走 journald
```

---

## 7. 模块设计

> 目录结构（规划，不含代码）：

```
cmd/linkc-node/              main、CLI（run/check/dump-config/version）
internal/app/                装配、生命周期、信号、supervisor
internal/config/             本地 YAML、校验、环境变量、热加载
internal/panel/              soga-v1 客户端
  models.go                  请求/响应 DTO
  client.go                  HTTP、TLS、重试、限流
  etag.go                    ETag/304 缓存
  contract.go                字段校验与归一化
internal/node/               NodeSpec、哈希、diff、reconcile 计划
internal/xray/               Xray 运行时
  builder.go                 NodeSpec → JSON/proto
  runtime.go                 core.Instance 生命周期、inbound 热切换
  users.go                   用户 diff/Add/Remove
  stats.go                   counter 读取/清零、online map
  gatekeeper.go              自定义 outbound 注册与配置
internal/gatekeeper/         限速、设备、TCP、审计匹配
  limiter.go                 token bucket
  device.go                  设备计数
  conns.go                   TCP 连接计数
internal/audit/              规则解析、匹配、去重、上报队列
internal/traffic/            采集、journal、批量推送
internal/online/             在线 IP 快照与变更
internal/status/             CPU/mem/swap/disk/uptime
internal/store/              bbolt 封装、迁移、锁
internal/scheduler/          pull/push/collect 定时器、jitter、backoff
internal/admin/              unix socket、healthz、metrics、pprof
internal/observability/      slog、Prometheus、脱敏
internal/version/            build info
test/mockpanel/              mock soga-v1 面板
test/e2e/                    真实 Xray 客户端联调
docs/                        运维与配置文档
```

### 7.1 Panel Client（soga-v1 客户端）

**职责**：

- 构造并发送 8 个接口请求；统一 Header。
- 处理 ETag/304、超时、重试、退避、限流、响应大小限制。
- 将面板 JSON 归一化为内部模型；校验失败不污染当前运行配置。
- 记录每个 endpoint 的最近成功时间、错误分类与指标。

**接口契约（设计级）**：

| 方法 | 输入 | 输出 | 失败语义 |
|---|---|---|---|
| `FetchNode(ctx)` | — | `NodeSpec` + ETag | 无缓存且失败 → 启动失败；有缓存 → 保留 |
| `FetchUsers(ctx)` | — | `[]UserSpec` + ETag | 保留旧列表 |
| `FetchAuditRules(ctx)` | — | `[]AuditRule` + ETag | 保留旧规则 |
| `FetchWhiteList(ctx)` | — | `[]string` + ETag | 保留旧白名单 |
| `PushTraffic(ctx,batch)` | `[]Traffic` | `{code,message}` | code!=0 视为失败 |
| `PushAliveIP(ctx)` | `[]AliveIP` | 同上 | 可重试/丢弃策略 |
| `PushAuditLog(ctx)` | `[]AuditLog` | 同上 | 保留队列重试 |
| `PushStatus(ctx)` | `Status` | 同上 | 记录指标 |

**关键行为**：

- `base_url` 末尾斜杠归一化，但**不改写路径大小写/不添加 query**，避免破坏面板的授权 URL 绑定。
- 禁止自动跟随重定向（默认），否则授权路径可能失效；可配置允许同源 307/308。
- 每个 endpoint 独立 backoff：1s → 2s → 4s → … → 5min（带 ±20% jitter）。
- `401/403`：视为配置/授权错误，5 分钟退避 + 告警，不删除已有用户。
- `404`：`/node` 404 表示面板未实现必需接口；无缓存时退出，有缓存时告警。
- `429`：尊重 `Retry-After`。
- 响应体上限（默认 32 MiB），防止面板返回异常大对象拖垮内存。
- 请求体分片：`traffic` 每批 ≤500 用户或 ≤1 MiB；`alive_ip` 每批 ≤500 用户；`audit_log` 每批 ≤500 条。
- 所有请求带 `User-Agent: linkc-node/<version>`。

**站点 profile（`sspanel-uim-soga`）**：

- 自动识别/显式配置；应用 §3.9.10 的默认值（`allow_empty=true`、`status.mode=load_fraction`、`audit.rule_mode=auto`、`audit.dedupe_ttl=24h`、`alive_ip.clear_offline=false` 等）。
- `node_type`、`traffic_rate`、`method/protocol/...` 等额外字段直接忽略；但 `node_type` 若存在必须为 `vless`，否则拒绝应用。
- 站点不下发 `ech_key`/`decryption`/`xhttp_settings`：缺失即按关闭处理，不要因此拒绝节点。
- 空 `users` 是业务结果，必须按空列表应用。
- `status` 发送 `load` + 文档字段双兼容 payload（见 §7.8）。
- 站点 POST 失败返回 `{code:1}`，按失败处理；GET 401/403/503 按退避处理。
- 站点无幂等键：traffic 重试可能重复计费/重复累加节点带宽，需在运维文档中明确。

### 7.2 Node Reconciler（配置协调器）

**职责**：比较「期望状态」与「已应用状态」，生成最小变更计划。

**变更分类**：

| 分类 | 触发字段 | 应用方式 |
|---|---|---|
| A. 仅调度参数 | `basic.pull_interval` / `push_interval` / `speed_limit` | 更新定时器/全局限速器，不碰 Xray |
| B. 用户集合 | `/users` | 增量 Add/Remove/更新，不重启 |
| C. 审计规则 | `/audit_rules` + `/white_list` | 重新编译 matcher，atomic swap |
| D. 入站参数 | `port` / `stream_type` / `tls_type` / `flow` / `decryption` / REALITY / XHTTP / ECH / 证书引用 | inbound 热切换（D10） |
| E. 进程级配置 | 日志级别、DNS、路由、API 等 | 整实例重建（MVP 不由面板触发） |

**哈希**：对规范化后的 NodeSpec 计算 SHA-256。仅当 D 类字段的哈希变化才触发热切换；A 类单独比较。

**应用顺序**（一次 reconcile）：

1. 校验并归一化所有输入；失败 → 记录并保持现状。
2. 若节点参数变化：生成新配置 → 构建新 inbound → 预检（端口/证书/key 格式）→ 切换 → 回滚点保存。
3. 若用户变化：diff → 对旧 inbound/新 inbound 分别应用 → 更新本地表。
4. 若审计变化：编译 → 校验 → swap。
5. 持久化 `last_good`（节点参数 + 用户 + 规则 + 白名单）。
6. 更新指标与日志。

**并发控制**：全局 `applyMu` 串行化所有应用操作；周期拉取与手动触发共用。

### 7.3 Xray Runtime

**启动**：

1. 若无缓存且首次拉取失败 → 进程退出（systemd 重试）。
2. 构建基础配置：log / stats / policy / direct(gatekeeper) / block / inbound。
3. `core.New(core.Config)` → `Start()`。
4. 通过 `inbound.Manager` 获取 handler，注册用户（配置中已带，或启动后批量 Add）。
5. 校验 stats/online 可用（读取 level 0 策略）。

**inbound 热切换**：

- 新 tag 命名：`vless-<generation>`；旧 tag 保留到新 inbound 就绪。
- 使用 `core.AddInboundHandler(instance, cfg)`；Xray 在监听时设置 `SO_REUSEPORT`，同端口可重叠。
- 成功后 `inbound.Manager.RemoveHandler(oldTag)`；若移除失败，记录并继续（旧监听可能仍在）。
- 失败路径：
  1. 若因端口占用失败：先移除旧 inbound，再添加新；仍失败则用旧配置重建。
  2. 若因证书/REALITY 参数失败：直接放弃新配置，不触碰旧 inbound。
- **同端口重叠窗口**：`SO_REUSEPORT` 会让内核在两份 listener 间分发，配置切换后仍可能有极少量新连接落到旧 listener（新旧 REALITY/TLS 参数不一致时会握手失败）。该窗口应控制在 <1s；M0 需验证 `RemoveHandler` 对存量连接的影响，并评估是否接受该抖动。
- 用户：新 inbound 从配置带全量用户；随后同步最新 diff；旧 inbound 移除后其用户自然消失。
- 流量采集在切换前后各执行一次，避免计数器丢失（计数器属于实例，不随 inbound 消失）。
- 证书：配置 `certificateFile/keyFile` 且 `oneTimeLoading=false`（文件方式默认）时，Xray 会按小时热加载证书；无需重启 inbound。
  - **M0 发现**：文件热加载在 `-race` 下与握手存在上游 data race（`transport/internet/tls`）。生产建议证书变更时使用 inline PEM + inbound 热切换，不依赖该 ticker。

**实例重建**：

- 触发：核心 panic 恢复失败、进程级配置变化、Xray 版本升级。
- 顺序：采集+journal → 停止旧实例（关闭监听/连接）→ 用 last_good 构建 → 启动 → 恢复用户 → 继续。
- 预期断线：秒级；文档说明。

**日志**：

- Xray 日志级别 `warning`；`access` 默认 `none`。
- 错误日志输出到 stdout，由 systemd/journald 收集；如需文件，配置路径并由外部 logrotate 管理。
- 审计不依赖 access log（Gatekeeper 在内存中匹配）。

### 7.4 用户同步器

**数据映射**：

| 面板字段 | Xray 用户字段 | 说明 |
|---|---|---|
| `id` | `email = "u" + id` | stats/online 反查键 |
| `uuid` | `account.id` | 校验 UUID；规范化 |
| `speed_limit` | Gatekeeper 表 | Mbps → bytes/s |
| `device_limit` | Gatekeeper 表 | 0=不限 |
| `tcp_limit` | Gatekeeper 表 | 0=不限；目标站点恒为 0 |
| 节点 `flow` | `account.flow` | 所有用户统一 |
| — | `level = 0` | 统一策略等级 |

**diff 算法**：

- 维护 `uuid → email`、`email → UserSpec` 两张表。
- 新增：`AddUser`；若 email 已存在但 UUID 变化 → 先 `RemoveUser(email)` 再 `AddUser`（避免 validator UUID 索引覆盖后旧 email 残留）。
- 删除：先采集并 journal 该用户流量 → `RemoveUser(email)` → 清理本地 limit 表；online map 会随存量连接关闭自然清除；counter 可选 `UnregisterCounter` 回收。
- 无变化：仅更新 limit 元数据（不需要动 Xray）。
- 批量：每批 100~500 个操作，避免长时间持锁；应用期间新连接使用旧用户集，最终一致。
- 安全阀（`panel.profile=sspanel-uim-soga`）：
  - **空用户列表默认应用**（站点在全员超流量、节点超带宽时会合法返回 `[]`），仅告警 + 指标；如确需拒绝，设 `users.allow_empty=false`（不推荐本站）。
  - 单次删除比例 >50% 或 >1000 人时**仅告警**（过期/超流量/超带宽均可能合法）；`mass_remove_guard=reject` 可切严格模式。
  - 非法用户（UUID 解析失败、重复、id<=0）跳过并计数；若非法比例 >10% → 整批拒绝。
  - 用户数上限（默认 100,000），超出拒绝。
  - 站点 `tcp_limit` 恒为 0，可忽略；`keep_connect` 超流量用户会以 `speed_limit=1` 保留。

**UUID 细节**：

- Xray 的 `ProcessUUID` 会把 UUID 的第 6、7 字节清零（`time_hi_version` 字段，而非 variant 字节）。检测去重时必须按同样规则归一化；发现冲突则跳过后来者并告警。
- 邮箱不能包含 `>>>`（stats 名分隔符）；`u<id>` 天然安全。
- 邮箱大小写：Xray validator 统一小写，保持一致。

**传输/安全组合校验**（不合法的组合必须在应用前拒绝，避免客户端配置与节点不一致）：

| 组合 | 结果 | 说明 |
|---|---|---|
| `tcp` + `tls`/`reality` + `vision` | 允许 | 推荐；TLS 1.3；REALITY 仅 RAW/XHTTP/GRPC |
| `tcp` + `none` + `vision` | 拒绝 | vision 必须有 TLS/REALITY |
| `tcp`/`ws` + `none`，无 flow | 允许（告警） | 明文 VLESS，易被识别；仅测试/内网 |
| `ws` + `tls`，无 flow | 允许 | ALPN 自动 http/1.1 |
| `ws` + `reality` | 拒绝 | 当前 Xray 不支持 |
| `ws`/`grpc`/`xhttp` + `vision` | 拒绝 | vision 仅支持 RAW(TCP)+TLS/REALITY |
| `grpc` + `tls`/`reality`，无 flow | 允许 | 有弃用警告；ALPN h2 |
| `xhttp` + `tls`/`reality`，无 flow | 允许 | 映射 xhttpSettings；ALPN h2（或本地覆盖） |
| `http` / `h2` | 拒绝（默认） | 现代 Xray 已移除；可选映射 XHTTP（不等价旧客户端） |
| `tls_type=tls` 但无证书 | 拒绝 | 本地证书配置缺失 |
| `flow` 非 `xtls-rprx-vision` | 拒绝 | Xray 仅支持该值或空 |
| vision + UDP | 运行期拒绝 | VLESS vision 不支持 UDP；需客户端关闭 UDP 或不用 vision |

### 7.5 流量采集与上报

**采集**：

- 对当前用户表中的每个 email：
  - `user>>>u<id>>>traffic>>>uplink`
  - `user>>>u<id>>>traffic>>>downlink`
- `counter.Set(0)` 原子返回旧值；两个方向分别处理。
- 若 counter 不存在（用户尚无流量），跳过。
- 采集周期默认 30s；必须 ≤ `push_interval`。
- 采集结果追加到 journal（bbolt），并更新内存中的待上报聚合（可选）。

**上报**：

- 按 `seq` 顺序读取 journal；聚合同一用户的多条记录为一个 `{id,u,d}`。
- 站点 `SogaController::traffic()` 的实际行为：
  - `u/d` 为增量，按 `node.traffic_rate` 倍率计入用户 `u/d`（节点 `node_bandwidth` 累加原始值）；
  - `ss_node_online_log.online_user = count(payload)`，即**在线数来自 traffic 条数**，不由 `alive_ip` 更新；
  - 无幂等键，重试会同时重复累加用户与节点流量。
- 因此默认只发送**有增量**的用户（不发送 0/0 条目）；站点在线数因此是“本轮有流量的用户数”的近似。如需要准确在线数，可开启 `traffic.include_online_zero=true`，把当前在线用户以 `u/d=0` 一并发送（代价：每轮写 0/0 `user_traffic_log`）。
- 分片 POST；每片成功后标记 `acked`（或推进水位线）。
- `code == 0` 才算成功；`code != 0` 记录 `message` 并保留。
- 失败重试：指数退避，上限 5min；不阻塞其他 endpoint。
- 全量成功后清理已确认记录；journal 超过上限时：
  - 优先丢弃最旧的 **已确认** 记录；
  - 未确认记录永不丢弃，超过硬上限（如 64MB）则停止采集并告警（避免内存/磁盘打爆）。

**重启/崩溃**：

- journal 持久化；启动后先恢复待上报数据，再开始新采集。
- 核心重启前后各采集一次；`core_epoch` 变化时，旧 epoch 的待上报数据继续保留。
- 已知风险：推送成功但响应丢失 → 重启后重发 → 面板重复计费。文档标注为 at-least-once；如面板支持幂等键，可通过扩展 header 解决（不在协议内）。

**对账**：

- 本地可选记录 `up_total/down_total`（仅调试），便于与面板核对。
- 提供 admin `/status` 显示 `pending_bytes`、`last_success_at`、`last_error`。

### 7.6 在线 IP

- 每 `push_interval`（或独立 15s）遍历 `user>>>u<id>>>online` 的 `OnlineMap`。
- 过滤 loopback（Xray 已排除），可选过滤私网；IPv6 去除 zone。
- 生成 `[{id, ips}]`，按配置：
  - `full`：每次全量发送所有在线用户；
  - `changed`：仅发送与上次快照不同的用户 + 每 N 次心跳全量。
- **目标站点不清除离线 IP**：`SogaController::aliveIp()` 只做 60s 内 upsert，不删除旧记录；`ips: []` 不会清理任何行。因此 `alive_ip.clear_offline` 默认 **false**；不要依赖该接口做下线清理。
- 分片推送；失败重试；不持久化（在线状态是瞬时的）。
- 设备数限制以该数据为参考，Gatekeeper 的实时计数为准。

### 7.7 审计引擎

**规则解析**（详见 §10.4）：

- 编译为 `domainMatcher`、`ipMatcher`、`portList` 的组合。
- 复用 Xray 的 `common/geodata`，支持 `geosite:` / `geoip:`。
- 正则规则单独存放；限制数量与长度（防 ReDoS）。
- 规则 ID 唯一；重复 ID 拒绝。
- 规则热更新：编译成功才替换；失败保留旧规则并告警。

**匹配时机**：

- Gatekeeper `Process` 入口，此时：
  - `session.Inbound.User.Email` 已知；
  - `session.Outbound.Target` 已被 sniffing 改写为域名（`routeOnly=false`）或保留 IP；
  - 目标端口已知。
- 先查白名单：命中 → 跳过审计；
- 再按面板顺序匹配审计规则：第一条命中即生效（与 soga 语义一致）。
- 规则匹配结果可按 `(domain, port)` 做短 TTL 缓存（规则全局，不区分用户），降低高频同域名开销。

**动作**：

- `block`：不拨号，直接关闭连接（或走黑洞语义）；记录命中。
- `report`：正常放行，仅记录命中。
- 去重：`(user_id, audit_id)` 在 `dedupe_ttl` 内只上报一次；持久化，重启后仍生效。
- 上报：事件队列 + debounce（1s）批量 POST；失败重试；同一批次去重。

**白名单**：

- 本地文件（每行一条）+ 面板列表合并；文件变更在下一个 pull 周期生效；
- 支持注释/空行；
- 与审计规则同语法；
- 白名单优先于审计规则。

**局限**：

- 依赖 sniffing；ECH/ESNI、HTTP/3、纯 IP 访问可能看不到域名；
- IP 规则对域名目标需要本地解析（可配置开启）；
- 正则规则数量多时 CPU 升高，需限制与缓存；
- 审计阻断会影响客户端体验，建议面板侧同时记录/封禁。

### 7.8 状态采集

| 字段 | 来源 | 备注 |
|---|---|---|
| CPU 百分比 | `/proc/stat` 两次采样差值 | 上报“上次推送至本次”的平均值；首次用 1s 窗口或 0 |
| CPU 小数 `load` | CPU 百分比 / 100 | **本站必填**；站点按 `load[0]*100` 显示百分比 |
| mem.total/used | `/proc/meminfo` | used = MemTotal - MemAvailable |
| swap.total/used | `/proc/meminfo` | 无 swap 时为 0 |
| disk.total/used | `statfs(data_dir)` | 可用配置指定路径；容器内为容器文件系统 |
| uptime | `/proc/uptime` | 主机 uptime，不是进程 uptime |

**站点兼容 payload（`status.mode=load_fraction`，默认）**：

```json
{
  "load": "0.235",
  "cpu": 23.5,
  "mem":   { "total": 1073741824, "used": 536870912 },
  "swap":  { "total": 0, "used": 0 },
  "disk":  { "total": 1073741824, "used": 536870912 },
  "uptime": 123456
}
```

- 站点只持久化 `load`（varchar(32)）和 `uptime`；`load` 第一个 token 必须是 CPU 小数（0..1），否则前端会把 `23.5` 显示成 `2350%`。
- `cpu`/`mem`/`swap`/`disk` 保留给文档型面板；本站忽略。
- `status.mode=doc` 时仅发送文档字段（不推荐本站，会导致负载显示错误）。
- 采样器每 5s 缓存一次；推送时取最近值。
- 采集失败：字段填 0 + 告警，避免整次推送失败。
- 容器场景：默认读容器视角；提供 `status.host_metrics: true` 时尝试读取 host `/proc`（挂载依赖）。

### 7.9 调度器

| 任务 | 默认 | 约束 |
|---|---|---|
| 拉取 node/users | 60s（面板 `pull_interval`） | 10s ~ 3600s |
| 拉取 audit/whitelist | 同 pull（可独立） | 同上 |
| 采集流量 | 30s | 10s ~ push |
| 推送 traffic | 60s（面板 `push_interval`） | 10s ~ 3600s |
| 推送 alive_ip | 同 push | 同上 |
| 推送 status | 同 push | 同上 |
| 审计上报 | 事件驱动 + 1s debounce | 最大 500/批 |

- 所有周期加 ±10% jitter，避免面板侧同时被打。
- 单 endpoint 使用 singleflight，禁止重叠请求。
- 面板返回新的间隔后，下个周期生效，无需重启。

### 7.10 管理面

- Unix socket（默认 `/run/linkc-node/admin.sock`，0600）：
  - `GET /healthz`：进程存活；
  - `GET /readyz`：核心已启动、最近一次配置应用成功、面板最近成功时间在阈值内；
  - `GET /status`：节点 id、核心状态、用户数、在线数、待上报流量、最后错误、版本；
  - `GET /metrics`：Prometheus；
  - `GET /debug/pprof/*`：可选，默认关闭。
- CLI：
  - `linkc-node run -c config.yaml`
  - `linkc-node check -c config.yaml`（只校验本地配置/证书/目录权限）
  - `linkc-node dump-config -c config.yaml`（拉取面板并输出生成后的 Xray 配置，脱敏）
  - `linkc-node version`
  - `linkc-node gen-reality-key` / `gen-ech-key`（可选工具）
- SIGHUP：重载本地配置（不重启核心；需要时触发 reconcile）。
- SIGUSR1：dump goroutine/堆栈（可选）。

### 7.11 部署与运维

**systemd（推荐）**

- 单实例：`linkc-node.service`；多节点：`linkc-node@<node>.service`（每实例独立配置/状态目录）。
- 关键项：
  - `Restart=on-failure`、`RestartSec=3`、`StartLimitIntervalSec=0`；
  - `LimitNOFILE=1048576`；
  - 非 root 用户；仅低端口时 `AmbientCapabilities=CAP_NET_BIND_SERVICE`；
  - `StateDirectory=linkc-node`、`RuntimeDirectory=linkc-node`；
  - `NoNewPrivileges=yes`、`ProtectSystem=strict`、`ProtectHome=yes`、`PrivateTmp=yes`、`PrivateDevices=yes`；
  - `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`。
- 日志：默认 journald；如需文件，使用 logrotate（copytruncate）或外部采集。

**Docker**

- 推荐 `network_mode: host`（避免端口映射与源 IP 丢失；源 IP 对在线 IP/设备限制很重要）。
- 需要绑定 443 时加 `NET_BIND_SERVICE`；无需 `NET_ADMIN`（本方案不限速到内核层）。
- 挂载：配置文件只读、证书只读、`/var/lib/linkc-node` 可写。
- 只读根文件系统（`read_only: true`）+ tmpfs。

**升级/回滚**

1. 备份 `state.db`、本地配置、证书（证书可不在备份内）。
2. 替换二进制；systemd 重启。
3. 启动时执行 bbolt schema 迁移（向后兼容；迁移前自动备份）。
4. 若失败：回滚旧二进制；若 schema 已迁移，恢复备份。
5. 版本信息写入 `meta`，防止旧版本打开新 schema。

**备份**

- 必备份：`config.yaml`、`state.db`（含 last_good、流量 journal）、白名单。
- 不建议备份：节点缓存中的临时数据。
- 恢复后首次启动会立即拉取面板，避免使用过期配置。

**系统调优（可选）**

- BBR/fq：提升吞吐（`net.core.default_qdisc=fq`、`net.ipv4.tcp_congestion_control=bbr`）。
- `fs.file-max`、`net.core.somaxconn`、`net.ipv4.ip_local_port_range` 按规模调整。
- 时间同步（chrony/systemd-timesyncd）：TLS/REALITY 依赖正确时间。

**多实例与端口**

- 同一台机器多节点时，每个实例必须使用不同端口（或不同 IP）。
- `NODE-ID` 不同、状态目录不同、admin socket 不同。
- 不支持同一实例内多 `NODE-ID`（MVP）。

**上线检查清单**

- [ ] `linkc-node check` 通过；
- [ ] 面板 `/node` 返回的传输/安全组合在支持矩阵内；
- [ ] 证书链/私钥匹配、REALITY 私钥可解析；
- [ ] 端口未被占用、防火墙放行 TCP/UDP（按传输）；
- [ ] 面板 API key 与服务端一致；
- [ ] 真实客户端逐传输验证；
- [ ] 流量/在线/状态上报可在面板侧看到；
- [ ] 面板断网演练：节点继续服务并恢复后追平；
- [ ] 重启演练：journal 恢复、用户自动同步。

---

## 8. soga-v1 接口实现规范

### 8.1 URL 与鉴权

- 最终 URL = `base_url`（去掉末尾 `/`）+ `/api/v1/...`。
- 保留 `base_url` 的原始路径、大小写与尾部斜杠语义，不做规范化之外的改写。
- `API-KEY` 支持环境变量/文件读取；不落日志；内存中尽量少拷贝。站点同时接受 query `?key=`，但只使用 header（避免日志泄露）。
- `NODE-ID` 字符串原样发送；本地校验非空。站点优先按 `NODE-ID` 查节点，缺失/0 时才回退到源 IP；若 `checkNodeIp=true`，源 IP 必须在该节点 `node_ip` 内。
- `NODE-TYPE` 固定 `vless`；若本地配置为其他类型，`check` 阶段报错。站点大小写不敏感。

### 8.2 ETag / 304

- 每个 endpoint 独立缓存：`{etag, body, fetched_at, sha256}`。
- 请求带上一次响应的 `ETag` 原值（包括 `W/` 前缀）。
- `304`：使用缓存 body；刷新 `fetched_at`；不触发 reconcile。
- `200`：
  - 有 ETag → 保存；
  - 无 ETag → 保存 body，清除旧 ETag（下次无条件 GET）；
- 缓存丢失但收到 304 → 立即发起一次无条件 GET。
- 缓存同时写入 bbolt 与内存；启动时先读内存/磁盘缓存。
- `ETag` 变化但 body 相同（sha256 一致）→ 不触发变更，仅更新缓存。

### 8.3 请求/响应校验

| 对象 | 校验 |
|---|---|
| port | 1~65535；与本地 listen 组合 |
| stream_type | 枚举；未知 → 失败 |
| tls_type | 枚举；未知 → 失败 |
| UUID | 标准解析 + `ProcessUUID` 归一化；重复冲突 → 跳过 |
| id | int64 > 0；重复 → 跳过 |
| 文本字段 | 长度上限（path 4096、serviceName 256、SNI 253）；禁止 NUL/CRLF |
| base64/hex | REALITY 私钥 32B；shortId 偶数长度 ≤16 hex；ECH key 格式待确认 |
| xhttp extra | 仅接受白名单字段；限制嵌套深度/大小；`headers` 不允许 `Host` |
| audit rule | 长度 ≤4096；未知前缀按策略处理 |
| IP 列表 | 每用户 ≤256；非法 IP 跳过；站点会去掉 `::ffff:` |
| 空用户数组 | **合法**（站点全员超流量/节点超带宽），按 `users.allow_empty` 应用；不要当错误 |
| 额外字段 | `node_type`、`traffic_rate`、`method/protocol/...` 忽略，不报错；`node_type` 若存在必须为 `vless` |
| status | 站点需 `load` 第一 token 为 CPU 小数；校验 0..1，`uptime`≥0 |
| 响应大小 | ≤32 MiB |
| 数组长度 | users ≤100k；rules ≤5000；whitelist ≤10000 |

### 8.4 错误处理与重试矩阵

| 场景 | 动作 |
|---|---|
| DNS/TCP/TLS 失败 | 重试 + 退避；保留现状 |
| 超时 | 同上 |
| 401/403 | 5min 退避 + 告警；不删除用户；不退出（站点 403 也可能是 `checkNodeIp` 不匹配） |
| 503 | 站点 `WebAPI=false` / 维护；退避 + 告警；保持服务 |
| 404 `/node` | 无缓存 → 退出；有缓存 → 告警 + 保持 |
| 404 其他 GET | 视为面板未实现；该能力禁用 + 告警 |
| 429 | 遵守 `Retry-After`（默认 60s） |
| 5xx | 退避重试 |
| 200 但 JSON 非法 | 视为失败；保留旧数据 |
| POST `code=1`（站点 `update failed`） | 失败；保留 journal/队列；记录 message，退避重试 |
| POST 200 但 body 非法 | 失败；重试 |
| GET `/users` 返回 `[]` | **成功**；应用空用户列表（站点配额/带宽语义） |
| 配置校验失败 | 不应用；保留 last-known-good；指标+日志 |
| 写盘失败 | 停止采集（保内存）、告警；继续服务 |
| 核心启动失败 | 回滚旧配置；连续失败则退出交给 systemd |

**幂等性**：

- GET 天然幂等（ETag）。
- POST 无幂等键；traffic 采用本地 journal + 确认后才推进水位；重复风险在文档中明确。
- 可选扩展：为 POST 增加 `X-Linkc-Batch-Id`（面板可忽略），为未来幂等实现留口。

### 8.5 时序与并发

- 拉取循环与推送循环独立；互不阻塞。
- 单 endpoint 同时最多 1 个在途请求（singleflight）。
- reconcile 与用户同步串行；采集可并发。
- 停止时：先停止拉取，再执行一次采集 + 推送（5s 超时），最后关闭核心。

---

## 9. 核心流程与状态机

### 9.1 状态机

```
        ┌─────────┐
        │  BOOT   │  读取本地配置/打开 store/校验
        └────┬────┘
             │ 有 last_good?
      ┌──────┴───────┐
      ▼              ▼
┌────────────┐  ┌──────────┐
│ START_FROM │  │  FETCH   │  首次拉取（无缓存）
│  CACHE     │  │  CONFIG  │
└─────┬──────┘  └────┬─────┘
      │              │ 成功
      └──────┬───────┘
             ▼
       ┌───────────┐
       │  RUNNING  │◀──────────────┐
       └─────┬─────┘               │
             │ 面板连续失败         │ 成功
             ▼                     │
       ┌───────────┐         ┌─────┴─────┐
       │ DEGRADED  │────────▶│  SYNCING  │
       └─────┬─────┘         └─────┬─────┘
             │                     │ 应用变更
             │                     ▼
             │               ┌───────────┐
             │               │ APPLYING  │
             │               └─────┬─────┘
             │                     │ 成功/回滚
             └─────────────────────┘
             │ SIGTERM
             ▼
       ┌───────────┐
       │ STOPPING  │  最终采集/推送 → 关闭核心 → 关闭 store
       └───────────┘
```

### 9.2 启动流程

1. 解析本地配置；校验权限、证书路径、目录可写、API key 非空。
2. 打开/初始化 bbolt（schema 迁移、单实例锁）。
3. 读取 last_good 与 ETag 缓存。
4. 有 last_good → 构建核心并启动（stale-while-revalidate）；否则先拉取 `/node`（失败则退出）。
5. 立即拉取 `/users`、`/audit_rules`、`/white_list` 并应用。
6. 启动采集/推送/审计/健康检查循环。
7. 输出启动日志（版本、node_id、端口、传输、安全类型、用户数）。

### 9.3 节点配置变更

```
panel 返回新 /node
   │
   ├─ 仅 A 类字段变化 → 更新 scheduler/global limiter
   │
   ├─ D 类字段变化
   │     ├─ 归一化 + 校验 + 生成新 JSON
   │     ├─ 构建新 inbound
   │     ├─ AddInboundHandler(new tag)
   │     │     ├─ 成功 → RemoveInbound(old tag) → 保存 last_good
   │     │     └─ 失败（端口占用）
   │     │            ├─ RemoveInbound(old) → AddInbound(new)
   │     │            │      ├─ 成功 → 保存
   │     │            │      └─ 失败 → 用旧配置重建 → 告警
   │     │            └─ 其他错误 → 保留旧 inbound → 告警
   │     └─ 采集流量前后各一次
   │
   └─ E 类字段变化 → 整实例重建（不在 MVP 面板控制范围）
```

### 9.4 用户变更

```
新 users 列表
   │
   ├─ 校验/去重/归一化
   ├─ 计算新增/删除/变更集合
   ├─ 对删除用户：采集流量 → RemoveUser → 清理 counter/online/limit
   ├─ 对新增用户：AddUser（配置 email/uuid/flow/level）
   ├─ 对变更用户：RemoveUser(old email) → AddUser(new)
   ├─ 更新本地 UserSpec/limits 表
   └─ 保存 last_good(users)
```

### 9.5 面板不可达

- `RUNNING` → 连续 N 次失败（默认 3）→ `DEGRADED`。
- 保持当前用户与配置；继续采集/journal；推送重试。
- 恢复后立即拉取并 reconcile；`DEGRADED` 期间不应用半成品/非法配置；空用户列表按站点 profile（本站允许）应用。
- 超过 `max_stale`（默认 24h）仍保持服务，但每 10min 告警；是否停止服务由配置决定（默认继续）。

### 9.6 优雅退出

1. 停止所有拉取/推送定时器（保留一次最终推送窗口）。
2. 执行一次流量采集 + journal 落盘。
3. 尽力推送 traffic/alive_ip/audit_log/status（总超时 5s）。
4. 关闭 Xray 实例（停止监听，关闭连接）。
5. 关闭 bbolt；释放文件锁。
6. 退出码 0。

### 9.7 崩溃恢复

- 进程崩溃：systemd 重启；bbolt 恢复 journal；先恢复流量上报，再按 last_good 启动核心。
- 核心崩溃：supervisor 捕获（core.Start 错误 / 健康探测失败）→ 采集+journal → 用 last_good 重建 → 恢复用户。
- 磁盘满：bbolt 写入失败 → 停止采集、告警；继续提供代理服务；面板推送继续尝试。

---

## 10. 限额、审计与流量归属

### 10.1 限速（`speed_limit`）

| 范围 | 字段 | 语义 | 实现 |
|---|---|---|---|
| 节点 | `basic.speed_limit` | 整节点 Mbps | 全局共享 token bucket |
| 用户 | `users[].speed_limit` | 每用户 Mbps | 每用户共享 token bucket（双向独立） |

- 单位换算：`bytes/s = Mbps * 1_000_000 / 8`。
- 桶容量：`max(64 KiB, rate * 0.2s)`，可配置。
- 实现位置：Gatekeeper 对 `link.Reader`（上行）与 `link.Writer`（下行）加包装；`WaitN(ctx, n)` 按字节消费。
- **必须关闭 splice**：对有限速的用户设置 `session.InboundFromContext(ctx).CanSpliceCopy = 3`（Vision 的零拷贝路径会绕过包装器）。
- 0 表示不限；节点+用户同时配置时取更严格者。
- 精度：受 TCP 窗口与 burst 影响，目标误差 ≤10%；可在文档中说明。

### 10.2 设备数（`device_limit`）

- 语义：同一用户当前活跃的**不同源 IP 数**上限。
- 实现：
  - Gatekeeper 维护 `userID → map[ip]refCount`；
  - 新连接若 IP 不在集合且 `len(set) >= limit` → 拒绝（不拨号，关闭）。
  - 连接结束（Process 返回）时递减 refCount。
- IPv6：默认按完整地址计数；可配置 `/64` 聚合（推荐，避免隐私地址导致误判）。
- NAT：多个设备共享出口 IP 只计 1；需在文档中说明。
- 与 Xray OnlineMap 的关系：上报用 Xray；enforce 用 Gatekeeper；两者定期对账，偏差写入指标。
- 模式：`enforce` / `report` / `off`。

### 10.3 TCP 连接数（`tcp_limit`）

- 语义：同一用户并发 TCP 请求数上限。
- 实现：Gatekeeper 在 `Process` 入口按 `ob.Target.Network == TCP` 计数；超过则拒绝。
- **Mux 注意**：VLESS XUDP/Mux 会把多个逻辑请求复用到一条 TCP 连接；每逻辑请求都会触发 Dispatch，因此计数的是“逻辑 TCP 流”，与物理连接数不同。需在文档中明确，并可配置 `count_mux_streams`（默认 true）。
- UDP 不计入 tcp_limit。

### 10.4 审计规则语法（站点兼容）

> 目标站点 `SogaController::auditRules()` 返回 `detect_list` 的 `regex ?: text`，**裸值、无 `regexp:` 前缀**；后台 UI 把该字段标为“规则正则表达式”。因此解析器必须支持裸 regex，同时兼容文档型前缀。

| 形式 | 识别方式 | 匹配对象 |
|---|---|---|
| 已知前缀（`regexp:`, `domain:`, `full:`, `keyword:`, `geosite:`, `ip:`, `geoip:`, `port:`） | 按前缀 | 对应 matcher |
| 裸值且含正则元字符（如 `.*google\.com`、`^ads?\.`） | 视为 regex | 域名正则（Go RE2） |
| 裸值且无元字符（如 `google`） | 视为 keyword | 域名子串 |
| `port:1-1024,8000` | 端口列表 | 目标端口 |
| `ip:` / `geoip:` | IP/CIDR/geodata | 目标 IP（域名目标可配置本地解析） |
| `geosite:` | geodata 分类 | 域名分类 |

解析模式（`audit.rule_mode`）：

- `auto`（本站默认）：先识别已知前缀；剩余值含正则元字符 → regex，否则 keyword。
- `regex`：所有规则按正则解释（最符合本站后台“规则正则表达式”的语义）。
- `keyword`：所有规则按子串解释。
- `prefixed`：仅接受前缀语法，裸值跳过并告警。

其他规则：

- 站点 `detect_list.type`（明文/hex）不返回，无法用于匹配；仅按 `rule` 字符串处理。
- 白名单同语法；白名单优先；本地文件 + 面板列表合并。
- 未识别/编译失败：默认跳过该条 + 告警；`strict_rules=true` 时整批拒绝并保留旧规则。
- 正则使用 Go RE2（线性时间），仍限制 pattern 长度、规则数量与缓存规模。
- 规则缓存：`(domain, port)` → 命中规则 ID（TTL 30s），规则更新时整体清空。
- **需要实测确认**：真正的 soga 对裸 `.*google\.com` 是按 regex 还是 keyword 解释；建议先在测试节点用一条规则验证，再决定 `rule_mode`（auto 是保守默认）。

### 10.5 审计事件与上报

```
命中规则
  │
  ├─ 白名单命中? → 跳过
  ├─ (user, rule) 在 TTL 内已上报? → 跳过
  ├─ 入队 audit_queue（bbolt / 内存）
  ├─ action=block → 关闭连接
  └─ debounce 1s → 批量 POST /audit_log
        ├─ code=0 → 标记已上报，写 dedupe
        └─ 失败 → 保留重试
```

- 文档要求“同一用户多次触发同一审计规则只记录一次”；目标站点按 **自然日 + node_id** 去重（`detect_log`）。我们的本地去重默认 24h（`audit.dedupe_ttl=24h`），可配置为永久；即使本地漏判，面板仍会按日去重，不会写重行。
- `audit_log` 的 `audit_id` 为面板规则 id；规则更新后旧 id 可能失效，命中旧 id 的事件仍按原样上报（面板应忽略未知 id）。
- 站点 `DetectLog` 写入失败无 try/catch，可能返回 500；按普通 POST 失败重试即可。
- 站点侧可能根据 `detect_log` 触发审计封禁（`detect_ban_log`）；节点只负责上报与可选的单连接阻断，不应自行封禁用户（除非产品明确要求本地 ban）。

### 10.6 流量归属

- `u` = 用户上传 = Xray `user>>>...>>>traffic>>>uplink`（客户端→节点方向）。
- `d` = 用户下载 = `downlink`。
- 统计点在 dispatcher 的 link 包装层，包含代理协议解密后的负载字节；不包含 TLS/传输层开销。
- **目标站点**：`u/d` 为增量，用户计数乘 `node.traffic_rate`；`node_bandwidth` 累加原始 `u+d`；`online_user = count(payload)`。详见 §3.9.5。
- 被审计阻断的连接仍会产生少量 uplink/downlink 计数；与 soga 行为可能不同，需在联调时确认。
- 流量计数与 Gatekeeper 限速互不影响；限速丢包不计入额外字节。
- 站点无幂等键；重试会重复累加用户与节点流量，面板侧节点带宽限制可能提前触发。
- **分片与在线数**：站点每个 `/traffic` 请求都会写一条 `ss_node_online_log`，`online_user=count(payload)`；如果把一次推送拆成多片，站点在线数会变成“最后一片的用户数”，而不是总在线数。本站默认**单次 POST**（仅当超过 1 MiB/HTTP 限制再拆）；如必须拆且需要准确在线数，可在最后补发一条仅含在线用户的 0 流量请求（需开启 `traffic.include_online_zero`）。

---

## 11. 安全设计

### 11.1 密钥与凭据

- `API-KEY` 支持环境变量 `PANEL_API_KEY` 或文件（0600）；不在日志/指标/`/status` 中出现。
- 面板 TLS 默认严格验证；支持自定义 CA、SNI、mTLS；禁止 `InsecureSkipVerify` 以外的弱化（如确需，显式配置并告警）。
- 不自动跟随重定向；防止 API key 被带到其他主机。
- REALITY 私钥、ECH 私钥、用户 UUID 仅存内存与 bbolt（0600）；`dump-config` 默认脱敏。
- 日志中对 UUID 做掩码（前 8 位 + `…`），对 API key 完全隐藏。

### 11.2 输入信任边界

- 面板被视为可信但可能出错：所有字段严格校验（枚举/范围/长度/格式）。
- 禁止面板控制：本地文件路径、监听地址、日志路径、API socket、证书路径、执行命令。
- `xhttp_settings.extra` 采用白名单字段映射，拒绝未知/危险键（如 `downloadSettings` 指向任意地址）。
- REALITY `dest` / XHTTP `host` 校验：`host:port`、禁止 NUL/CRLF；可选禁止私网目标（防 SSRF），默认允许（REALITY 常用本地回落）。
- 审计规则：限制长度/数量；Go regexp 为 RE2 线性时间，但仍限制 pattern 长度、规则数量与缓存规模。
- 响应体限流与大小上限；HTTP 客户端设置 `MaxIdleConns`、`ResponseHeaderTimeout`。

### 11.3 运行环境

- 非 root 用户；`CAP_NET_BIND_SERVICE` 仅绑定低端口时添加。
- systemd 加固：`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、`PrivateTmp`、`PrivateDevices`、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`、`ReadWritePaths=/var/lib/linkc-node /run/linkc-node`。
- 不使用 shell；不写临时可执行文件。
- bbolt 单实例文件锁，防止重复启动。
- 管理 socket 目录 0700，文件 0600。

### 11.4 合规

- 仅实现公开文档描述的互操作协议，不复制 soga 代码/资源。
- Xray-core 为 MPL-2.0（文件级 copyleft）：引用未修改的源码不要求开源本项目；若修改 Xray 文件，需公开被修改文件的源码（本方案避免修改）。
- soga 为商业产品，替换其节点端可能受其授权条款约束；上线前请做法律/条款确认（尤其“授权码绑定 webapi_url”条款）。
- 本项目自身许可证待定（建议 Apache-2.0 或 MIT，需确认）。

---

## 12. 可观测性

### 12.1 日志

- `slog` JSON；字段：`ts, level, component, event, node_id, duration_ms, status, error, user_hash`。
- 关键事件：启动、配置应用成功/失败、用户批量变更摘要、面板请求失败、流量推送失败、核心重启、审计命中、限额拒绝。
- 不在 info 级别打印每个连接；debug 级别可开启，但默认脱敏。
- 重复错误限流（相同错误 1 分钟内只打一次，计数在指标中）。

### 12.2 指标（Prometheus）

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `linkc_panel_requests_total` | counter | endpoint, code | 面板请求 |
| `linkc_panel_request_duration_seconds` | histogram | endpoint | 延迟 |
| `linkc_panel_last_success_timestamp` | gauge | endpoint | 最近成功 |
| `linkc_config_apply_total` | counter | result | 应用成功/失败/回滚 |
| `linkc_core_up` | gauge | — | 核心健康 |
| `linkc_users` | gauge | — | 当前用户数 |
| `linkc_online_ips` | gauge | — | 在线 IP 数 |
| `linkc_traffic_pending_bytes` | gauge | — | 待上报流量 |
| `linkc_traffic_reported_bytes_total` | counter | — | 已上报 |
| `linkc_audit_hits_total` | counter | action | 审计命中 |
| `linkc_audit_pending` | gauge | — | 待上报审计 |
| `linkc_limit_rejects_total` | counter | reason | 限速/设备/TCP 拒绝 |
| `linkc_gatekeeper_active_conns` | gauge | — | 活跃连接 |
| `linkc_rate_limit_wait_seconds` | histogram | direction | 限速等待 |
| `linkc_store_write_errors_total` | counter | — | 持久化错误 |
| `process_*`, `go_*` | — | — | 标准 |

### 12.3 健康检查

- `healthz`：进程存活。
- `readyz`：核心启动 + 最近一次配置应用成功 + 最近一次面板拉取成功在 `3 × pull_interval` 内。
- `/status` 返回 JSON，便于人工排查（不包含密钥/UUID 全量）。

### 12.4 告警建议

- 面板连续失败 > 5 分钟；
- 核心 down > 30 秒；
- 流量 pending > 100MB 或 > 1 小时未成功上报；
- 配置应用失败；
- 磁盘使用 > 90%；
- 用户同步大面积失败/删除。

---

## 13. 性能与容量

### 13.1 目标

| 指标 | 目标 |
|---|---|
| 用户数 | 10k（设计上限 100k） |
| 并发连接 | 5k（设计上限 20k） |
| 吞吐 | 1 Gbps+（限速关闭时） |
| 内存 | 基础 < 120MB；10k 用户 + 5k 连接 < 300MB |
| CPU 开销 | 限速/审计关闭时 <3%；开启后 <10%（视规则量） |
| 用户同步 | 10k 用户 diff+apply <500ms |
| 流量采集 | 10k 用户 <50ms / 轮 |
| 配置切换 | 同端口重叠 <1s 抖动 |
| 面板请求 | 每 endpoint 1 次/周期，带 jitter |

### 13.2 热点与优化

- 用户查找：`sync.Map` 或分片 map；避免全局锁。
- 限速桶：仅对配置了限额的用户懒创建；连接结束不销毁（按用户复用），定期清理已删除用户。
- 审计匹配：编译后的 matcher + 结果缓存；正则规则单独处理。
- 日志：采样/限流；避免每连接日志。
- bbolt：批量事务；journal 顺序写；定期 compact（低频）。
- 内存：避免把完整面板响应长期保留两份；缓存 body 可直接作为 last_good。
- GC：避免每包分配；Gatekeeper 包装器复用 buffer 路径。

### 13.3 压测方法

- mock panel 提供 10k/50k 用户；
- 使用 Xray 客户端（多进程）建立 3k~5k 连接，持续 30 分钟；
- 记录 CPU、RSS、goroutine、面板请求延迟、流量误差；
- 对比限速开/关、审计开/关、splice 开/关。

---

## 14. 测试计划

### 14.1 单元测试

- soga→Xray 字段映射（每个字段、每个枚举、非法值）。
- NodeSpec 哈希/变更分类。
- 用户 diff（新增/删除/UUID 变更/重复/非法/空列表/大规模删除）。
- 流量增量计算（清零、重启、缺失 counter、负值、溢出）。
- ETag/304/缓存丢失/无 ETag。
- 重试/退避/jitter/429。
- 审计规则解析/匹配（所有前缀、白名单优先级、未知前缀、geosite/geoip）。
- 限速器（速率/突发/双向）、设备计数（IPv4/IPv6/NAT）、TCP 计数（mux/UDP）。
- 状态采集（无 swap、容器、缺文件）。
- 配置校验（端口、UUID、base64、REALITY、XHTTP）。

### 14.2 契约测试（mock panel，按站点源码）

mock panel 必须复刻目标站点 `SogaController` 的实际行为，而不仅是公开文档：

- 完整 8 个端点 + Header（API-KEY/NODE-ID/NODE-TYPE/IF-NONE-MATCH）；
- ETag 为 `"sha1(json)"`，精确匹配 304；
- 401/403/503/404/500/超时/慢响应/非法 JSON/超大响应；
- `/node` 额外字段 `node_type`/`traffic_rate`；VLESS config 仅上述子集（无 ECH/decryption/xhttp_settings）；
- `/users`：空数组合法、超流量跳过、`keep_connect` 限速 1、`tcp_limit=0`、`device_limit=node_connector`、额外字段；
- `/audit_rules`：裸 regex/text；`/white_list`：env 逗号分隔；
- `/traffic`：增量、倍率、`online_user=count(payload)`、`code=1` 失败；
- `/alive_ip`：ips/ip、`::ffff:`、60s upsert、不清除；
- `/audit_log`：同一自然日去重；
- `/status`：`load` 第一 token ×100、只存 load/uptime。
- Golden fixtures 用站点源码行为生成（不要用线上真实用户数据）；至少覆盖：
  - 空用户列表；
  - 超流量 + keep_connect；
  - 节点超带宽；
  - `load` 为 `"0.235"` 与误发 `"23.5"` 的对比；
  - 裸 regex 审计规则；
  - 离线 IP 不再发送后的 alive_ip 状态。

### 14.3 集成测试

- 真实 Xray 客户端（或 sing-box）通过本节点：
  - VLESS + TCP + TLS；
  - VLESS + TCP + REALITY + Vision；
  - VLESS + WS + TLS；
  - VLESS + gRPC + TLS；
  - VLESS + XHTTP（h2/http1.1）；
  - `none` 安全（仅测试环境）。
- 校验：
  - 客户端可连通 echo 服务；
  - `traffic` 上报与客户端实际收发字节误差 ≤1%；
  - `alive_ip` 正确；
  - 审计命中/白名单；
  - 删除用户后新连接被拒；
  - UUID 变更；
  - 设备/TCP/限速生效；
  - 配置热切换期间其他用户不中断；
  - 核心重启后 journal 恢复。

### 14.4 兼容性测试

- Xray `v26.11.9`（主）以及 `v26.9.30`（最低 ECH JSON）。
- 覆盖 `h2/http` 拒绝路径、`ws/grpc` 弃用警告。
- `soga-v1` 字段版本：`decryption`（v2.13.7+）、`ech_key`（v2.17.1+）。
- 目标站点：归档源码 commit / 部署版本核对；至少对 `SogaController.php` 做行级 diff，防止线上版本漂移。

### 14.5 故障注入

- 面板：全挂、间歇 500、慢响应、304 后变更、错误 ETag。
- 网络：DNS 失败、TLS 证书过期、连接重置。
- 磁盘：满、只读、bbolt 损坏（从备份恢复）。
- 核心：panic/启动失败/端口占用/证书损坏。
- 重启：kill -9、systemd 重启、双实例竞争文件锁。
- 时钟：NTP 跳变（证书/日志）。

### 14.6 安全测试

- 面板返回超长字符串/大量规则/嵌套 extra；
- Header 注入、路径穿越（本地路径不可控）；
- 日志泄露检查；
- 权限检查（0600/0700）；
- 依赖漏洞扫描（govulncheck）。

### 14.7 验收标准（摘要）

1. mock panel 下全接口通过，ETag 304 生效。
2. 真实客户端 5 种传输组合连通。
3. 流量误差 ≤1%，重启后无丢失（允许 at-least-once 小幅重复）。
4. 面板不可达 1h，节点持续服务；恢复后自动追平。
5. 用户增删 ≤1 个 pull 周期生效，其他用户不断线。
6. 非法节点配置不生效，旧配置继续服务。
7. 限速误差 ≤10%，设备/TCP 超限被拒。
8. 审计命中上报一次（TTL 内），白名单放行；站点自然日去重不产生重复行。
9. 10k 用户 + 3k 连接压测 30min 无泄漏/崩溃。
10. systemd 下重启/升级可恢复。
11. 目标站点联调：`/users` 空列表可正常应用；`status.load` 显示为正确百分比；`traffic` 的 `online_user` 与预期一致（或按配置解释）；`alive_ip` 可在站点在线 IP 表中看到。

---

## 15. 里程碑与任务拆解

> 估时按 1 名熟悉 Go/Xray 的工程师，单位：人日；含自测，不含外部等待。

### M0 · 技术验证与契约澄清（3-4 人日）— ✅ 核心完成

> 结果与证据见 **`M0-REPORT.md`**；spike 代码在 `internal/m0/*`，测试命令 `go test ./...`。

- [x] 内嵌 Xray，启动 VLESS + TCP；TLS；REALITY + Vision（真实客户端 E2E）。
- [x] JSON → `core.Config` → 注入 Gatekeeper outbound → 运行（M0 用 outbound.Manager 包装；生产可改 typed config）。
- [x] 直接调用 `UserManager` 增删用户；stats 读取/清零；OnlineMap 读取。
- [x] Gatekeeper 限速（含 Reality/Vision）/设备/TCP 逻辑；splice 关闭验证。
- [x] 同端口 inbound 热切换（SO_REUSEPORT）验证。
- [~] XHTTP 参数映射已测；ECH key 未测（站点不下发 ECH）。
- [x] 按站点 `SogaController` 复刻 mock panel：ETag/304、空 users、status.load、裸 regex、online_user、alive_ip 60s upsert。
- [x] 真实站点联调（2026-11-14，节点 ID 4，REALITY + Vision，证据见 `LIVE-VALIDATION.md`）；审计裸 regex 语义已由规则 23 实测确认。
- [x] 用站点源码差量核对，主要契约已写入 §3.9。

**退出标准（已达成）**：PoC 能跑通 1 个 VLESS 节点（TCP/TLS/Reality），真实客户端可连，流量/在线/状态可上报到 mock panel。
**M0 遗留**：均已在 M2–M5 与真实站点联调中关闭（status.load 口径、审计 regex 语义、证书来源=本地文件、空 users 行为）。

### M1 · MVP（5-7 人日）— ✅ 核心完成

> 结果见 **`M1-REPORT.md`**；生产代码在 `cmd/linkc-node` + `internal/*`。

- [x] 本地配置、校验、CLI、日志、bbolt。
- [x] Panel Client：GET node/users、ETag、重试；POST traffic/status；`sspanel-uim-soga` profile。
- [x] NodeSpec → Xray 配置（TCP/WS/TLS/REALITY）；status 双字段（load + cpu/mem）。
- [x] 用户同步（含 diff/安全阀）。
- [x] 流量采集/journal/推送；状态上报。
- [x] systemd unit、Dockerfile。
- [x] 基础文档。

**退出标准（已达成）**：真实 Xray 客户端 + mock panel 完成端到端；面板故障可保持服务（含新连接）；last_good 可在面板仍关闭时重启。
**M1 遗留**：真实站点联调；审计/alive_ip（M2）；XHTTP/GRPC/ECH（M2）；多证书/SIGHUP（M4）；`include_online_zero` 未实现。

### M2 · 协议完整 + 可靠性（5-6 人日）— ✅ 核心完成

- [x] XHTTP/GRPC 实际 TLS 传输 E2E；ECH/decryption 配置映射（ECH 实际握手待 key set）。
- [x] audit_rules/white_list 拉取与解析；block/report；白名单；去重队列。
- [x] alive_ip 采集与推送（changed/full）。
- [x] 全部 GET 的 ETag；错误矩阵；分片；幂等风险处理（traffic at-least-once）。
- [x] last_good/缓存/崩溃恢复；admin `/status`。
- [x] 审计/alive/流量指标；告警建议仍在文档层。
- [x] `traffic.include_online_zero`。

**退出标准（已达成）**：8 个接口全部实现并通过 mock；真实站点 node/users/traffic/status/audit_rules/white_list/audit_log/alive_ip 已跑通；`status.load` 已确认；XHTTP/GRPC 实际连接通过。
**M2 可选遗留**：ECH 实际握手（缺测试 key set）。

### M3 · 限额与审计执行（5-6 人日）— ✅ 核心完成

- [x] 用户/节点限速；设备限制（含 IPv6 /64 聚合）；TCP 限制；enforce/report/off。
- [x] 审计 block/report；TTL 去重；规则热更新；geosite/geoip；域名解析 IP 规则。
- [x] 限速/审计对 splice 的影响处理；Node 全局 limiter。
- [x] 真实站点 1 Mbps / 1 设备验收；[~] 多出口 IPv6 待补（10k 用户/吞吐压测已完成，见 M5）。

**退出标准（已达成）**：限额执行逻辑、设备/IPv6、TCP、审计白名单/去重/geosite 已通过单元测试；真实站点低限额（1 Mbps）与设备数（1）已验证。

### M4 · 加固与交付（4-5 人日）— ✅ 完成（M5 收尾）

- [x] inbound 热切换事务化：添加失败保留旧配置；旧移除失败自动回滚到旧 inbound（`Engine.SwapInbound`，含「新 inbound 也摘不掉」与「回滚本身失败」的兜底路径，单测覆盖 5 条分支）。
- [x] 多证书/SNI、SIGHUP 证书热加载；真实 TLS 轮换 E2E 已补（`TestReloadCertsRotatesServingCertificate`）；顺带修复 `Spec.Xray` 丢弃 `node.tls.certificates` 的 bug（`TestSpecPassesLocalCertificates`）。
- [x] 安全加固（systemd/权限/umask/输入校验/脱敏）；审计报告见 `docs/security-audit.md`。
- [x] 故障注入/混沌：面板 5xx/429、webapi 503、journal 写满、端口切换、用户 churn、证书轮换均有测试；长稳 harness 就绪（`make soak`，`SOAK_DURATION=24h`）。
- [x] Makefile、安装脚本、nfpm 模板、升级/回滚文档；deb/rpm 打包实测通过（安装/升级/卸载、权限、conffile）。
- [x] 运维/FAQ/指标文档；Prometheus 告警规则 `deploy/alerts.yml` + `docs/monitoring.md`；新增 `linkc_audit_pending` 指标。

**退出标准（已达成，除标注外）**：可部署、可运维、可备份回滚；完整回滚（deb/rpm 安装/升级/卸载）已实测。仍待验收：24h 长稳实跑、多域名证书真实场景、systemd 沙箱/SELinux 目标机兼容性（见 `docs/security-audit.md` S1/S2）。

**总计**：约 22~28 人日（不含缓冲）。建议预留 20% 缓冲。

---

## 16. 风险登记册

| # | 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|---|
| R1 | 站点源码已明确主要语义；审计裸 regex、status 口径仍待实测 | 中 | 中 | 用站点源码写契约测试；M0 用真实节点验证 |
| R2 | 现代 Xray 不支持 `h2/http`；面板仍下发 | 中 | 中 | 启动时明确拒绝并告警；提供 `xhttp` 迁移说明；不静默降级（XHTTP stream-one 与旧 h2 传输并不等价） |
| R3 | `ws/grpc` 未来被移除 | 中 | 中 | 固定版本；抽象 transport 层；跟踪上游 |
| R4 | 限速/设备/TCP 非原生，行为与 soga 有差异 | 高 | 中 | 文档声明 + 指标 + 可配置 enforce/report；必要时 fork 升级为 P2 |
| R5 | 限速与 Vision splice 冲突 | 高 | 高 | 限速连接强制关闭 splice；压测验证 |
| R6 | 审计依赖 sniffing；ECH/QUIC 导致漏判 | 中 | 中 | 文档说明；IP/端口规则兜底；可选 access log 辅助 |
| R7 | 流量崩溃窗口重复/丢失 | 中 | 中 | journal + 采集频率；面板幂等扩展（未来） |
| R8 | 面板返回恶意/异常数据导致 OOM/阻塞 | 中 | 高 | 大小上限、校验、分片、超时 |
| R9 | Xray 版本升级导致配置/行为变化 | 中 | 高 | 固定版本 + 升级矩阵测试 + 回滚 |
| R10 | ECH key 格式不兼容（目标站点不下发 ECH） | 低 | 低 | 保留可选支持；M0 专项验证；失败降级告警 |
| R11 | 证书管理缺失（ACME） | 中 | 中 | MVP 外部文件 + 热加载；P2 可选 ACME |
| R12 | 端口 443/低端口权限 | 低 | 中 | systemd capability；文档 |
| R13 | 授权/合规风险 | 中 | 高 | 上线前法律确认；不复制 soga 代码 |
| R14 | 一进程多节点需求 | 低 | 中 | 架构按 node 抽象；后续 supervisor |
| R15 | 观测/告警缺失导致故障不可见 | 低 | 中 | 指标 + readyz + 告警建议 |
| R16 | `status.load` 口径不符，前端负载显示错误（23.5 → 2350%） | 中 | 中 | 默认 `load_fraction`；M0 验证站点显示 |
| R17 | 站点裸 regex 在 soga/本代理侧解释不一致，审计漏判/误判 | 中 | 中 | `rule_mode=auto`；测试规则；必要时改站点规则加前缀 |
| R18 | 空用户列表被安全阀错误拒绝，导致配额/超带宽逻辑失效 | 中 | 高 | 站点 profile 默认允许空列表；仅告警 |
| R19 | 线上站点版本与归档源码不一致（SogaController 被改） | 中 | 中 | 上线前 diff/回归；契约测试；保留兼容开关 |
| R20 | 站点不提供 xhttp_settings/ECH/decryption，误以为节点配置完整 | 中 | 中 | 缺失即关闭并告警；文档写明能力矩阵 |
| R21 | 站点无幂等键，traffic 重试导致节点带宽/用户流量重复累加 | 高 | 中 | 最小化重试；journal 确认；监控 node_bandwidth 异常 |
| R22 | Xray GitHub tag 不是合法 Go module 版本，依赖无法按计划固定 | 中 | 中 | M0 已改用 commit 伪版本 `v1.260327.1-0.20261109052709-...`；记录 release 映射 |
| R23 | Xray freedom 默认阻断私网目标，影响本地测试/内网访问 | 中 | 中 | 增加 `freedom.allow_private_targets`（默认 false）；M0 显式开启 |
| R24 | Xray 文件证书热加载与握手存在上游 data race | 中 | 中 | 生产用 inline PEM + inbound 热切换；记录上游问题 |

---

## 17. 待确认问题

> 标记 **P0** 的问题会影响架构，建议在 M0 前确认。

### 已由站点源码确认

- 本程序角色：节点侧 soga 替代品（WebAPI 客户端）；站点 `SogaController` 是服务端。
- 8 个端点、Header、ETag/304、错误码（401/403/404/503、POST code 1）。
- `traffic` 为增量；用户计数乘 `traffic_rate`，节点带宽累加原始值。
- `alive_ip` 按 60s 窗口 upsert，不清除离线 IP。
- `audit_log` 按自然日 + node + user + rule 去重。
- `status` 只持久化 `load`（第一 token ×100 显示）与 `uptime`。
- `users` 空数组合法；`tcp_limit` 恒 0；`device_limit=node_connector`。
- 站点不下发 `ech_key`/`decryption`/`xhttp_settings`。

### 真实站点验证（2026-11-14）

- 测试节点 ID 4（`94.237.36.107`），REALITY + Vision，已用真实 Xray 客户端跑通 HTTP。
- `/users` 4098、`/audit_rules` 27（全部裸正则，编译 27/27）、`/white_list` 0。
- 规则 23 命中 `www.360.cn`；audit_log 推送成功，本地去重生效。
- 测试账户 49720 跑通真实流量（剩余 1023.97MB → 1023.86MB）；traffic / status / alive_ip 均被站点接受；`pending_bytes` 归零。
- `include_online_zero=true` 时面板节点卡显示“1 在线”。
- `status.load` 已由站点侧确认显示正确（CPU 小数口径成立）。
- 详细记录见 `LIVE-VALIDATION.md`。
- 仍待确认：空 users、限额、真实面板断网、在线 IP 页面。

### P0（2026-11-14 联调后仍开放）

1. **部署版本**：线上站点是否与归档源码完全一致？是否有二次修改/插件？
2. ~~审计规则解释~~ ✅ 已确认：站点 27 条规则全部裸 regex，`auto` 模式编译 27/27，规则 23 实测命中 `www.360.cn`。
3. ~~状态口径~~ ✅ 已确认：`load` 第一 token 即 CPU 使用率（0..1），前端显示正确。
4. **在线数口径**：站点在线数来自 traffic 条数，是否要求包含零流量在线用户（会写 0/0 流量日志）？默认不包含（`include_online_zero=false`）。
5. **限额要求**：`speed_limit`/`device_limit` 强制执行已实现并验证；待确认面板侧是否还有额外口径（如超流量用户限速 1 Mbps 的展示）。
6. **传输范围**：站点是否配置 `http`/`h2`？现代 Xray 已移除，只能拒绝或让站点改用 `xhttp`；站点也不下发 `xhttp_settings`。
7. **证书来源**：`tls_type=tls` 时证书由本地文件、外部 ACME 工具还是其他方式？（站点不提供证书字段；REALITY 节点不涉及）
8. **`xtls` 配置**：站点把 `xtls` 映射为 `none`；后台是否有人这样配？建议改成 `reality` 或 `tls`。
9. **checkNodeIp / 反代**：实测从非节点 IP 直接 GET `/node` 成功，疑似关闭；节点在反代后时 `REMOTE_ADDR` 是否可信需确认。
10. **部署目标**：已按 Linux + systemd 交付（deb/rpm + Docker），端口按面板下发（实测 440）；是否需要 443 由站点配置决定。

### P1

11. 预期规模：用户数、并发连接、带宽、节点数量。
12. Xray 版本策略：是否接受固定某个版本？是否允许未来 fork？
13. 是否需要多节点聚合/集中管理？
14. 可观测性：是否已有 Prometheus/Grafana？
15. 本项目许可证选择？
16. 是否需要 ACME 自动证书？
17. 是否需要提供 `xray` 原生链接生成/校验工具？
18. 是否需要 ECH/decryption/xhttp 的本地扩展（站点不提供，但 Xray 支持）？

---

## 18. 附录

### 附录 A. 生成配置骨架（示意，不是可运行代码）

```jsonc
{
  "log": { "loglevel": "warning", "access": "none" },
  "stats": {},
  "policy": {
    "levels": {
      "0": {
        "handshake": 10,
        "connIdle": 180,
        "uplinkOnly": 2,
        "downlinkOnly": 2,
        "bufferSize": 16,
        "statsUserUplink": true,
        "statsUserDownlink": true,
        "statsUserOnline": true
      }
    }
  },
  "inbounds": [
    {
      "tag": "vless-1",
      "listen": "0.0.0.0",
      "port": 443,
      "protocol": "vless",
      "settings": {
        "decryption": "none",
        "flow": "xtls-rprx-vision",
        "clients": [
          { "id": "uuid", "email": "u1", "level": 0, "flow": "xtls-rprx-vision" }
        ]
      },
      "streamSettings": {
        "network": "tcp",
        "security": "reality",
        "realitySettings": {
          "target": "www.example.com:443",
          "serverNames": ["www.example.com"],
          "privateKey": "...",
          "shortIds": ["0123456789abcdef"],
          "xver": 0
        }
      },
      "sniffing": {
        "enabled": true,
        "destOverride": ["http", "tls", "quic"],
        "routeOnly": false
      }
    }
  ],
  "outbounds": [
    { "tag": "direct", "protocol": "freedom", "settings": { "domainStrategy": "UseIPv4v6" } },
    { "tag": "block", "protocol": "blackhole" }
  ],
  "routing": {
    "domainStrategy": "AsIs",
    "rules": [
      { "type": "field", "outboundTag": "direct", "network": "tcp,udp" }
    ]
  }
}
```

> 构建后处理：把 `direct` 的 `ProxySettings` 替换为 `GatekeeperConfig`（内含 freedom 配置）。

### 附录 B. 关键接口/源码索引（Xray v26.11.9）

| 主题 | 路径 |
|---|---|
| 流量/在线统计 | `app/dispatcher/default.go` |
| OnlineMap | `app/stats/online_map.go` |
| Stats API | `app/stats/command/command.proto` |
| 用户/入站管理 API | `app/proxyman/command/command.proto` |
| 运行时加入入站 | `core/xray.go`（`AddInboundHandler`） |
| VLESS 入站 | `proxy/vless/inbound/inbound.go` |
| VLESS 用户校验 | `proxy/vless/validator.go` |
| VLESS 配置 | `infra/conf/vless.go` |
| 传输枚举/移除 | `infra/conf/transport_internet.go` |
| TLS/REALITY/ECH | `infra/conf/transport_security.go` |
| XHTTP/GRPC/WS | `infra/conf/transport_method.go` |
| Policy | `infra/conf/policy.go` |
| splice | `proxy/proxy.go` |
| 监听 reuseport | `transport/internet/system_listener.go` |
| freedom | `proxy/freedom/freedom.go` |
| 路由 webhook（备选） | `app/router/webhook.go` |

### 附录 C. 备选方案对比

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| 内嵌 Xray + Gatekeeper | 单二进制、可限额/审计、无 fork | 进程内故障、需自定义 outbound | **推荐** |
| Xray 子进程 + gRPC | 故障隔离、易升级 | 无法连接级限速/审计；两进程 | 备选 |
| Fork Xray（dispatcher 限速） | 性能最好、行为精确 | 维护成本、安全更新慢 | P2 备选 |
| 使用 soga 本身 | 现成 | 商业授权、非 Xray | 不采用 |

### 附录 D. 若目标是面板侧

如果最终想要的是「实现这些接口给 soga 节点用」：

- 核心工作变成：HTTP 服务端 + 数据库（节点/用户/规则/流量账本）+ ETag 生成 + API key 鉴权 + 管理 UI。
- **不需要在面板里运行 Xray-core**；Xray 在节点侧（soga）。
- 本方案可复用的部分：协议模型、字段校验、ETag/错误语义、调度与运维经验。
- 需新增：节点注册、用户 CRUD、订阅生成、流量限额结算、审计处置、面板 UI/API。
- 架构、数据库、鉴权、部署需要单独规划。

### 附录 E. 参考资料

- soga-v1 WebAPI 文档：https://www.yougotme.cc/soga/doc/sogav1-webapi-kai-fa-wen-dang
- Xray-core 仓库：https://github.com/XTLS/Xray-core
- Xray-core 版本：`v26.11.9`（固定）
- `go.mod` 要求 Go 1.27
- XHTTP 讨论：https://github.com/XTLS/Xray-core/discussions/4113
- REALITY：`transport/internet/reality`
- 目标站点源码归档：`web_panel.testred.xyz_20261113_040001_3bFox4.tar.gz`（SSPanel-UIM + SogaController）
- 本规划中所有“源码依据”均以 `v26.11.9` 为准；升级需重跑兼容矩阵。

### 附录 F. 目标站点（SSPanel-UIM + SogaController）源码索引

归档：`web_panel.testred.xyz_20261113_040001_3bFox4.tar.gz`（2026-11-13）。归档内含站点配置与 `.git`，按敏感资料保管，禁止提交到代码仓库。

| 主题 | 文件/方法 |
|---|---|
| soga-v1 路由 | `web/app/routes.php`（`/api/v1/node` … `/api/v1/status`） |
| 端点实现 | `web/src/Controllers/Api/V1/SogaController.php` |
| 鉴权/节点查找 | `SogaController::guard()` / `findNode()` / `declaredNodeId()` |
| 节点配置 | `SogaController::nodePayload()` / `vlessConfig()` |
| 用户列表 | `SogaController::users()` / `queryNodeUsers()` |
| 审计规则 | `SogaController::auditRules()` + `App\Models\DetectRule`（表 `detect_list`） |
| 白名单 | `SogaController::whiteList()`（`$_ENV['soga_audit_white_list']`） |
| 流量/在线/状态 | `SogaController::traffic()` / `aliveIp()` / `auditLog()` / `status()` |
| VLESS 参数标准化 | `web/src/Utils/URL.php::normalizeVlessItem()` |
| node.server 解析 | `web/src/Utils/Tools.php::v2Array()` |
| 节点后台示例 | `web/resources/views/metron/admin/node/create.tpl` |
| 节点模型/状态 | `web/src/Models/Node.php`、`NodeInfoLog.php`、`NodeOnlineLog.php` |
| 流量/活跃 IP 模型 | `web/src/Models/TrafficLog.php`、`Ip.php` |

**VLESS `node.server` 示例（站点后台）**：

```
example.com;443;0;tcp;reality;sni=www.microsoft.com|flow=xtls-rprx-vision|fp=chrome|pbk=PUBLIC_KEY|sid=SHORT_ID|spx=/|private_key=PRIVATE_KEY|short_ids=SHORT_ID|server_names=www.microsoft.com|dest=www.microsoft.com:443
```

**站点兼容速查**：

| 项 | 站点行为 | 本代理默认 |
|---|---|---|
| `/node` 额外字段 | `node_type`、`traffic_rate` | 忽略 |
| `/node` VLESS 字段 | 无 ECH/decryption/xhttp_settings | 缺失=关闭 |
| `/users` 空数组 | 合法（超流量/超带宽） | `allow_empty=true` |
| `/users` `tcp_limit` | 恒 0 | 忽略 |
| `/status` | `load` 第一 token ×100 | `load_fraction` |
| `/audit_rules` | 裸 regex/text | `rule_mode=auto` |
| `/audit_log` 去重 | 自然日 | `dedupe=day` |
| `/alive_ip` | 60s upsert，不清除 | `clear_offline=false` |
| `/traffic` 在线数 | `count(payload)` | 默认不含 0 流量；可开 |

**联调前核对**：

1. 线上 `SogaController.php` 与归档源码是否一致（尤其 `status`/`users`/`auditRules`）。
2. 节点 `server` 字符串中的 `private_key`、`short_ids`、`server_names`、`dest` 是否完整。
3. 节点 `node_ip` 是否包含节点出口 IP（若 `checkNodeIp=true`）。
4. `API-KEY` 是否在 `muKey` 或 `muKeyList` 中。
5. 节点 `sort=15` 或请求头 `NODE-TYPE: vless`（站点两者都支持）。
6. `keep_connect` 是否符合预期（超流量用户限速 1 Mbps）。
7. 先发一条测试审计规则，确认站点/节点对裸 regex 的解释一致。

---

## 变更记录

| 版本 | 日期 | 变更 |
|---|---|---|
| v0.1 | 2026-11-14 | 初稿：角色假设、架构、接口、限额/审计、里程碑、风险 |
| v0.2 | 2026-11-14 | 依据目标站点源码（SSPanel-UIM + SogaController）补充 §3.9、站点 profile、status.load、空用户、裸 regex、alive_ip、在线数、附录 F；更新风险与待确认问题 |
| v0.3 | 2026-11-14 | M0 技术验证完成：嵌入 Xray、TLS/REALITY/Vision E2E、Gatekeeper 限速（splice）、用户热更、同端口热切换、mock 面板契约；修正 Go module 伪版本与 freedom 私网阻断；发现 Xray 文件证书热加载 data race；报告见 `M0-REPORT.md` |
| v0.4 | 2026-11-14 | M1 MVP 完成：配置/CLI/bbolt、soga-v1 客户端、NodeSpec/Xray、用户同步、流量 journal、状态、last_good、admin、systemd/Docker；TCP/WS+TLS E2E；报告见 `M1-REPORT.md` |
| v0.5 | 2026-11-14 | M2 部分完成：audit_rules/white_list 拉取与匹配、block/report、去重队列、alive_ip changed/full、include_online_zero、审计指标；报告见 `M2-REPORT.md` |
| v0.6 | 2026-11-14 | M2 核心完成：XHTTP/GRPC 实际 TLS E2E、ECH/decryption 配置、本地扩展；真实站点测试账户/在线数/审计验证；报告见 `M2-REPORT.md` |
| v0.7 | 2026-11-14 | M3 核心完成：节点全局限速、IPv6 /64 设备聚合、TCP/report/off、geosite/geoip、域名解析 IP 规则、限额指标；报告见 `M3-REPORT.md` |
| v0.7.1 | 2026-11-14 | M3 验收：真实站点 1 Mbps/1 设备通过；修复 inbound 热切换 oldTag 误删新 tag、ETag 304 与 last_good 不一致；新增回归测试 |
| v0.8 | 2026-11-14 | M4 工程化：多证书/SNI、SIGHUP 证书重载、systemd 加固、Makefile/安装脚本/nfpm、升级回滚/FAQ 文档；报告见 `M4-REPORT.md` |
| v0.9 | 2026-11-14 | M5 收尾：inbound 热切换事务化回滚（`Engine.SwapInbound` + 5 分支单测）；真实 TLS 证书轮换 E2E；修复 `Spec.Xray` 丢弃多证书的 bug；多证书 SNI E2E；混沌测试（面板 5xx/429、503、journal 写满）；10k 用户/吞吐压测与 benchmark；soak harness（`make soak`）；deb/rpm 打包实测；`deploy/alerts.yml` + `docs/monitoring.md` + `docs/security-audit.md`；新增 `linkc_audit_pending` 指标；报告见 `M5-REPORT.md` |
