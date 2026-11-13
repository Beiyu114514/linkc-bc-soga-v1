# M0 技术验证报告

> 对应规划：`PLAN.md` §15 M0
> 日期：2026-11-14
> 状态：**核心风险已验证通过；剩余为真实站点联调**
> 代码：`internal/m0/*`（spike，不是生产实现）

---

## 1. 结论摘要

| 验证项 | 结果 | 说明 |
|---|---|---|
| 内嵌 Xray-core 启动 | ✅ | v26.11.9（Go module 伪版本） |
| VLESS + TCP（无 TLS）E2E | ✅ | 真实 Xray 客户端 → 节点 → HTTP 目标 |
| VLESS + TLS E2E | ✅ | 自签证书 + `pinnedPeerCertSha256` |
| VLESS + REALITY + Vision E2E | ✅ | 本地 TLS 伪装目标；x25519 + shortId |
| 用户热添加/删除 | ✅ | `proxy.UserManager`，无需重启 |
| 流量 counter 读取/清零 | ✅ | `Set(0)` 增量；面板收到流量 |
| OnlineMap | ✅ | 非 loopback 源 IP 可见；loopback 被 Xray 忽略 |
| Gatekeeper 注入 | ✅ | 通过 outbound.Manager 包装 freedom |
| 用户限速 | ✅ | 4 Mbps 下 512KiB ≈ 0.72s；Vision 下同样生效（splice 已关闭） |
| TCP/设备限制逻辑 | ✅ | 单元测试；E2E 暂未覆盖 |
| 同端口 inbound 热切换 | ✅ | SO_REUSEPORT；新 listener 接客后移除旧 tag |
| mock 面板站点契约 | ✅ | ETag/304、空 users、status.load、裸 regex、traffic、alive_ip、audit_log |
| 真实站点联调 | ⏳ | 需要线上测试节点/API key |
| 审计规则实际语义 | ⏳ | 裸 regex 在 soga 侧的解释待验证 |
| 证书来源/ACME | ⏳ | 站点不下发证书 |

**结论**：架构中最危险的几件事（嵌入 Xray、Gatekeeper 注入、Vision 下限速、同端口热切换、站点 `status.load`/空 users 语义）都已经过可运行代码验证，没有出现“此路不通”的问题。M0 可以进入 M1。

---

## 2. 环境与依赖

- OS：Debian 13 (trixie)，linux/arm64。
- Go：`go1.27.3 linux/arm64`（`/usr/local/go/bin/go`）。
- Xray：GitHub release `v26.11.9`，commit `676eaf66f8f05d4d558ee7a74b4736623d3feeb9`。
- Go module 版本：`v1.260327.1-0.20261109052709-676eaf66f8f0`。
  - **注意**：`v26.11.9` 不是合法 Go module semver，`go get github.com/xtls/xray-core@v26.11.9` 会失败。必须用 commit 伪版本（或等上游发布对应 `v1.26xxxx` 模块 tag）。计划中“固定 v26.11.9”应改为“固定该 release 的 commit 伪版本”。
- 依赖：`golang.org/x/time/rate`、`golang.org/x/crypto/curve25519`、`golang.org/x/net/proxy`。

---

## 3. 已实现内容（spike）

```
internal/m0/
  mockpanel/   目标站点 SogaController 行为模拟（测试夹具）
  engine/      Xray 内嵌、配置生成、用户、stats/online、Gatekeeper、热切换
  node/        soga-v1 客户端 + 同步 + 流量/在线/状态推送
  e2e/         VLESS/TLS/REALITY/Vision/限速/热更 E2E
```

### 3.1 mock panel

按目标站点源码复刻：

- `GET /api/v1/node`：额外 `node_type`/`traffic_rate`；VLESS config 子集；ETag = `"sha1(json)"`；304。
- `GET /api/v1/users`：超流量跳过、`keep_connect` 限速 1、节点超带宽返回 `[]`、`tcp_limit=0`、额外字段。
- `GET /api/v1/audit_rules`：`regex ?: text` 裸值；`white_list`：列表。
- `POST /api/v1/traffic`：增量、`traffic_rate` 倍率、节点带宽原始值、`online_user=count(payload)`。
- `POST /api/v1/alive_ip`：60s 内 upsert，不清除离线 IP，`::ffff:` 归一化。
- `POST /api/v1/audit_log`：同 user+rule+node 自然日去重。
- `POST /api/v1/status`：`load`/`cpu`，只存 `load`+`uptime`。
- `API-KEY`、`NODE-ID`、`WebAPI=false`、`checkNodeIp`。

### 3.2 engine

- 用 JSON 生成 `core.Config`，`core.New` + `Start`。
- 启动后把 `direct`（freedom）从 `outbound.Manager` 取出，移除，换成本地 `wrapHandler`（只覆写 `Dispatch`，其余委托）。这样不需要为 Gatekeeper 生成 protobuf 配置，适合 M0；生产可改为 post-process typed config。
- Gatekeeper：用户/节点限速、TCP 计数、设备 IP 计数、审计 hook 位置（M0 未实现 matcher）。
- `CanSpliceCopy=3`：有限速时禁止 Vision 直连 splice，确保 link wrapper 不被绕过。
- 用户：`inbound.Manager` → `proxy.GetInbound` → `proxy.UserManager`，Add/Remove。
- 流量：`stats.Manager.GetCounter("user>>>u<id>>>traffic>>>uplink/downlink").Set(0)`。
- 在线：`GetOnlineMap("user>>>u<id>>>online").ForEach`。
- 热切换：`core.AddInboundHandler` 新 tag（同端口）→ `inbound.Manager.RemoveHandler` 旧 tag。

### 3.3 node

- `GET /node`、`/users` 带 `If-None-Match`，304 使用缓存。
- `node_type` 校验；空 users 正常应用。
- `POST /traffic`、`/alive_ip`、`/status`。
- 状态：`/proc/stat`、`/proc/meminfo`、`statfs("/")`、`/proc/uptime`。
- `status` payload 同时发 `load="0.xxxxxx"` + `cpu/mem/swap/disk/uptime`，兼容目标站点与文档型面板。
- 节点配置变化：inbound 参数变化 → 新 tag 热切换；仅用户变化 → 增量 Add/Remove。

---

## 4. 测试结果

命令：

```bash
cd /root/linkc-bc-soga-v1
/go/bin/go test ./... -count=1
```

结果：

```
ok  internal/m0/e2e       3.7s
ok  internal/m0/engine    0.05s
ok  internal/m0/mockpanel 0.03s
ok  internal/m0/node      0.05s
```

`go test -race ./... -count=1` 也全部通过（TLS 测试改用 inline PEM，规避下述上游 race）。
### E2E 明细

| 测试 | 结果 | 备注 |
|---|---|---|
| `TestVLESSPlainTrafficOnlineStatus` | PASS | HTTP through VLESS；流量上报；OnlineMap；status load |
| `TestVLESSTLS` | PASS | 自签证书 + pinned peer cert |
| `TestVLESSRealityVision` | PASS | REALITY + Vision + 4Mbps 限速；512KiB 0.72s |
| `TestUserHotRemoveAndAdd` | PASS | 移除后认证失败；加回后恢复 |
| `TestUserSpeedLimit` | PASS | 4Mbps；512KiB 0.72s |
| `TestInboundHotSwapSamePort` | PASS | 同端口 SO_REUSEPORT 新 listener 接客 |
| `TestNodeETagAndNodeTypeValidation` | PASS | 第二次同步 304；node_type 不匹配拒绝 |
| mock panel 8 项测试 | PASS | ETag、空 users、status、audit、traffic、alive IP、auth |

---

## 5. 重要发现（对计划的修正）

1. **Go module 版本固定方式**
   - 不能写 `require github.com/xtls/xray-core v26.11.9`；该 tag 不是合法 semver。
   - 使用 commit 伪版本，例如 `v1.260327.1-0.20261109052709-676eaf66f8f0`。
   - `PLAN.md` 的“固定 v26.11.9”应改为“固定 commit + 记录 release 版本”。

2. **Gatekeeper 注入方式**
   - M0 采用 `outbound.Manager.RemoveHandler/AddHandler` 包装已启动的 freedom handler，可行且无需 proto 配置。
   - 生产建议仍走“构建 core.Config 后替换 typed outbound”的路径（更干净、可 dump）；M0 证明两种路径都不需要 fork。
   - `RemoveHandler` 不会关闭 inner handler；wrapper 的 `Close` 委托 inner，避免泄漏。

3. **Vision/splice 与限速**
   - 只包装 link 不够：Freedom 会走 splice 直连，绕过 writer。
   - 在 Gatekeeper 中设置 `session.Inbound.CanSpliceCopy = 3` 后，REALITY+Vision 的限速 E2E 通过；这是生产实现必须保留的细节。
   - 关闭 splice 会牺牲部分零拷贝性能；无限制用户不应被强制关闭（M0 只在有限速时设置）。

4. **freedom 默认阻断私网**
   - Xray 26.x freedom 对 VLESS 入站默认阻断私网目标（防 SSRF）。
   - M0 为了本地测试加了 `finalRules: allow private`。生产要决定：
     - 默认保留阻断（更安全），还是允许用户访问内网；
     - 若保留，M0/E2E 的本地目标需要非私网或显式 allow。
   - `PLAN.md` 应增加 `freedom.allow_private_targets` 配置项，默认 `false`。

5. **OnlineMap 排除 loopback**
   - Xray 不记录 `127.0.0.1`/`::1` 源 IP；E2E 必须用非 loopback 本地 IP，生产监控要注意。
   - 设备数限制的 Gatekeeper 自己计数不受此影响（但 loopback 实际连接也会计入，需确认是否要排除）。

6. **status.load**
   - mock 验证了站点公式 `load[0]*100`；发送 `load="0.235000"` 得到 23.5%。
   - 线上 soga 的真实 `load` 语义仍未验证（CPU 使用率 vs load average）；需要用真实站点确认。

7. **traffic 在线数**
   - 站点 `online_user=count(payload)`，分片会拆成多条 `ss_node_online_log`，最后一片决定显示。
   - M0 默认只发有增量的用户，在线数是近似；如需准确，开启 `include_online_zero`。

8. **空 users 合法**
   - 站点在全员超流量/节点超带宽时返回 `[]`，必须应用空列表，不能当异常。

9. **裸 audit regex**
   - mock 只验证了“返回裸值”；真实 soga 对 `.*google.com` 是 regex 还是 keyword 尚未验证。
   - M0 未实现 audit matcher；M2 实现前必须先做一条规则的真实联调。

10. **`xtls` 映射**
    - 站点把 `tls=xtls` 映射为 `tls_type=none`；若 panel 配置 vision，会失败。生产应拒绝并提示管理员改用 `tls`/`reality`。

11. **Xray TLS 文件热加载的上游 data race**
    - 使用 `certificateFile/keyFile` 时，Xray 的证书热加载 goroutine 与握手读取证书存在 data race（`transport/internet/tls/config.go`，`-race` 可复现）。
    - M0 的 TLS E2E 改用 inline PEM（`certificate/key`，`OneTimeLoading=true`）后 race 通过。
    - 生产建议：证书变更时走“inline PEM + inbound 热切换”，不依赖 Xray 每小时的证书热加载；若坚持文件热加载，需接受该上游 race 风险。

---

## 6. 尚未覆盖 / 遗留

- 真实站点联调（最重要）：需要测试节点、API key、线上 `SogaController.php` 版本核对。
- audit matcher（domain/IP/port、geosite/geoip、白名单、去重）尚未实现。
- 持久化流量 journal、崩溃恢复、面板故障保持服务（M1）。
- 拉取/推送循环、间隔/jitter/backoff（M1）。
- 证书管理/ACME、证书热加载（M1/P2）。
- 多用户/大列表性能、10k 用户压力测试（M1/M4）。
- Gatekeeper `tcp_limit`/`device_limit` 的真实 E2E（当前单元测试通过）；限速已 E2E。
- 审计/白名单拉取尚未接入 Node。
- 生产构建的 Xray import 裁剪（`main/distro/all` 用于 spike）。

---

## 7. 对 PLAN.md 的更新建议

1. §3.7 版本固定：改为 commit 伪版本，注明 GitHub tag 与 Go module semver 的差异。
2. §5 D8 / §10.1：补充“仅在有限速时设置 `CanSpliceCopy=3`；已验证 Vision 下限速”。
3. §7.4：增加 `freedom.allow_private_targets`（默认 false）；M0 E2E 用 true。
4. §3.9.8：`status.load` 已验证 mock 行为；仍待真实站点确认。
5. §15 M0：大部分完成；剩余真实站点联调与 audit 语义确认。
6. 风险表：增加“Go module tag / 伪版本”“freedom 私网阻断影响测试”。

---

## 8. 真实站点联调清单（下一步）

1. 在站点后台创建测试节点：
   - 类型 VLESS，`sort=15`；
   - server 字符串填 `host;port;0;tcp;reality;sni=...|flow=xtls-rprx-vision|private_key=...|short_ids=...|server_names=...|dest=...`；
   - `node_ip` 填节点出口 IP（若 `checkNodeIp=true`）；
   - 记录 `NODE-ID` 与 `API-KEY`。
2. 启动 M0 node（或后续 M1 agent）指向站点，确认：
   - `/node`、`/users` 返回与 mock 一致；
   - 客户端能连；
   - `traffic` 后站点用户流量变化；
   - `status` 后站点“负载”显示为 23.5% 而不是 2350%；
   - `alive_ip` 出现在在线 IP 页；
   - 空 users 场景（超流量/超带宽）行为符合预期。
3. 创建一条审计规则（裸 regex），确认节点侧实际按 regex 匹配；再决定 `rule_mode`。
4. 确认 `checkNodeIp`、反代、证书来源。

---

## 9. 运行方式

```bash
cd /root/linkc-bc-soga-v1
export PATH=/usr/local/go/bin:$PATH
go test ./... -count=1
```

M0 spike 没有生产 CLI；验证通过 `go test` 驱动。M1 再引入 `cmd/linkc-node`。
