# 监控与告警

管理面通过 Unix socket 暴露 `/status` 与 `/metrics`（默认 `/run/linkc-node/admin.sock`）。
Prometheus 不能直接抓取 Unix socket，这里给出两种落地方式；告警规则见
[`deploy/alerts.yml`](../deploy/alerts.yml)。

## 1. 指标清单

| 指标 | 类型 | 含义 |
|---|---|---|
| `linkc_ready` | gauge | 1 = 已完成 bootstrap 且引擎在运行 |
| `linkc_degraded` | gauge | 1 = node/users 连续失败 ≥3 次 |
| `linkc_users` | gauge | 当前生效用户数 |
| `linkc_online_ips` | gauge | Xray OnlineMap 中的在线 IP 数 |
| `linkc_traffic_pending_bytes` | gauge | 待上报流量字节（journal 积压） |
| `linkc_audit_pending` | gauge | 待上报审计事件数 |
| `linkc_limits_mode{mode}` | gauge | 限额模式（enforce/report/off），恒为 1 |
| `linkc_node_speed_mbps` | gauge | 节点全局限速（Mbps，0 = 不限） |
| `linkc_active_tcp` | gauge | 活跃 TCP 连接数 |
| `linkc_active_devices` | gauge | 活跃设备数（IPv6 按 /64 聚合） |
| `linkc_limit_rejects_total{reason}` | counter | 限额拒绝数（tcp/device） |
| `linkc_audit_rules` | gauge | 已编译审计规则数 |
| `linkc_audit_hits_total` | counter | 审计命中数 |
| `linkc_audit_reported_total` | counter | 已上报审计数 |
| `linkc_audit_blocked_total` | counter | 审计阻断数 |

## 2. 抓取方式

### 方式 A：node_exporter textfile collector（推荐，无需改代码）

```cron
* * * * * root /usr/local/bin/linkc-node-metrics.sh
```

`/usr/local/bin/linkc-node-metrics.sh`：

```bash
#!/bin/sh
set -eu
SOCK=/run/linkc-node/admin.sock
OUT=/var/lib/node_exporter/textfile_collector
tmp=$(mktemp)
curl -sS --unix-socket "$SOCK" http://localhost/metrics > "$tmp"
mv "$tmp" "$OUT/linkc-node.prom"
```

指标名保持不变，`node` 标签可用 `relabel_configs` 按 instance 补齐。

### 方式 B：本地 TCP 代理

```bash
socat TCP-LISTEN:9109,fork,reuseaddr UNIX-CONNECT:/run/linkc-node/admin.sock
```

然后按普通 HTTP 目标抓取 `127.0.0.1:9109/metrics`。

## 3. 告警规则

```bash
promtool check rules deploy/alerts.yml
```

内置规则（严重度从高到低）：

| 告警 | 触发条件 | 严重度 |
|---|---|---|
| `LinkCNodeNotReady` | `linkc_ready == 0` 持续 5m | critical |
| `LinkCNodeDegraded` | `linkc_degraded == 1` 持续 15m | warning |
| `LinkCTrafficBacklog` | `linkc_traffic_pending_bytes > 100MB` 持续 15m | warning |
| `LinkCAuditBacklog` | `linkc_audit_pending > 1000` 持续 30m | warning |
| `LinkCNoUsers` | `linkc_users == 0` 持续 10m | warning |
| `LinkCLimitRejectsSpike` | 10m 内拒绝 > 100 | warning |
| `LinkCAuditBlockedSpike` | 10m 内阻断 > 100 | info |

按需调整阈值：`LinkCTrafficBacklog` 对应 PLAN §12.4 的「pending > 100MB 或
> 1 小时未成功上报」；未成功上报时长可由 `linkc_traffic_pending_bytes`
持续上升间接判断。

## 4. 手动检查

```bash
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
curl --unix-socket /run/linkc-node/admin.sock http://localhost/readyz
```

`/status` 还包含 `last_success`（各端点最后成功时间）与 `last_error`，
排障时优先看这两项。
