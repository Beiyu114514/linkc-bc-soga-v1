# M3 进展报告：限额与审计执行

> 对应规划：`PLAN.md` §15 M3
> 日期：2026-11-14
> 状态：**M3 验收通过**（真实站点 1 Mbps / 1 设备）

---

## 1. 限额（Gatekeeper）

### 1.1 节点限速

- 新增节点级全局限速器，所有用户共享一个 token bucket。
- 与用户限速器串联：连接同时受节点限速与用户限速约束。
- 节点限速变化时替换全局 limiter，后续新连接生效。
- 配置：`node.basic.speed_limit`（来自面板 `/node`）。

### 1.2 用户限速

- 每用户独立 limiter。
- `speed_limit=0` 不限制。
- 限速连接强制 `CanSpliceCopy=3`，关闭 Vision 零拷贝，避免绕过。
- 已验证：REALITY + Vision 4 Mbps 下 512KiB ≈ 0.72s。

### 1.3 设备数

- `device_limit` = 同一用户不同源 IP 数。
- IPv6 支持前缀聚合：
  - `node.limits.device_limit_ipv6_prefix`，默认 `128`；
  - 推荐 `/64`，避免隐私地址误判。
- IPv4-mapped IPv6 会先 unmap。
- 连接结束按引用计数递减。

### 1.4 TCP 连接数

- `tcp_limit` 按逻辑 TCP 流计数。
- `0` 表示不限；目标站点当前恒为 0。
- UDP 不计入。

### 1.5 模式

- `enforce`：限速/设备/TCP 全部执行。
- `report`：只计数，不拒绝、不限速。
- `off`：完全关闭 Gatekeeper 限制（审计仍独立执行）。

### 1.6 指标

`/metrics` 新增：

- `linkc_limits_mode{mode="..."}`
- `linkc_node_speed_mbps`
- `linkc_active_tcp`
- `linkc_active_devices`
- `linkc_limit_rejects_total{reason="tcp|device"}`

`/status` 同步包含这些字段。

---

## 2. 审计执行

### 2.1 已完成

- 规则前缀：`regexp:` / `domain:` / `full:` / `keyword:` / `ip:` / `port:` / `geosite:` / `geoip:`
- `auto` 裸值：明确正则元字符按 regex，否则 keyword
- 白名单优先；面板列表 + 本地文件合并
- `block` / `report`
- `(user, rule)` TTL 去重；bbolt 队列；`/audit_log` 推送
- 规则热更新；30s 匹配缓存
- `geosite.dat` / `geoip.dat` 加载：
  - `node.geodata.dir`
  - 缺失文件跳过并告警
- `resolve_domain_for_ip_rules`：域名目标上的 IP 规则可本地解析
- 审计与限额模式解耦

### 2.2 重要回归修复

- 真实站点在上线期间修改了 REALITY 参数，触发 inbound 热切换。
- 发现并修复：切换时 `oldTag` 在 `AddInbound` 之后才读取，误删新 inbound，导致后续 `ApplyUsers` 报 `handler not found`。
- 已增加 `TestInboundSwapThenUserApply` 覆盖“inbound 切换后继续增删用户”。
- 同时修复 ETag 304 缓存与 last_good 不一致时未重新应用 users/node 的问题。

### 2.3 限制

- geosite 属性（`@cn` 等）暂不参与匹配。
- `geoip` 的 reverse 语义未实现。
- DNS 解析使用系统 resolver，2s 超时 + 60s 缓存。
- ECH/ESNI、HTTP/3、纯 IP 目标可能看不到域名。

---

## 3. 测试

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

新增覆盖：

| 测试 | 结果 |
|---|---|
| `TestDeviceIPv6Aggregation` | ✅ /64 聚合与拒绝/释放 |
| `TestTCPLimitAndReportMode` | ✅ TCP 限制与 report 模式 |
| `TestNodeGlobalLimiter` | ✅ 节点全局 limiter |
| `TestGeoDataRules` | ✅ geosite/geoip 加载与匹配 |
| `TestResolveDomainForIPRule` | ✅ 域名目标上的 IP 规则本地解析 |
| `TestInboundSwapThenUserApply` | ✅ 修复 inbound 切换后旧 tag 误删新 tag 的回归 |
| `TestGateSpeedReplacesLink` | ✅ 用户限速包装 |
| `TestStartUserSpeedLimit` | ✅ 生产 runtime 4 Mbps 限速 E2E |
| `TestVLESSRealityVision` | ✅ Vision 下限速不绕过 |
| 审计 E2E（block/report/whitelist） | ✅ |

---

## 4. 真实站点状态

- 节点已升级到 `0.7.3-m3`（PID 1670，监听 440）。
- `/metrics` 已暴露 `limits_mode=enforce`、`node_speed_mbps`、`active_tcp`、`active_devices`、`limit_rejects_total`。
- 测试账户 49720 已限制为 `speed_limit=1`、`device_limit=1`，完成验收：
  - loopback 512KiB 3.48s，平均 1.2Mbps（含 burst，稳态约 1Mbps）；
  - 第二源 IP 连接 EOF，`rejects_device=1`，释放后恢复；
  - 测试后已恢复 `freedom.allow_private_targets=false`。
- 空 users/断网/多出口 IPv6 仍待演练。

## 5. 尚未完成 / 可选

1. 限额精度/吞吐压测（10k 用户、并发连接、节点全局限速）。
2. 真实面板断网、空 users、节点带宽阈值演练。
3. 多出口 IPv6 / NAT 设备数场景。
4. geosite 属性/reverse、规则热加载后的缓存细化。
5. Prometheus 告警规则模板。

---

## 6. 下一步

1. 真实站点创建低限额用户，完成 M3 验收。
2. 压测节点全局/用户限速与设备计数。
3. 进入 M4：inbound 完整回滚、证书/SIGHUP、多证书/SNI、打包与混沌测试。
