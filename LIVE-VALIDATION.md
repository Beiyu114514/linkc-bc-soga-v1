# 真实站点联调记录

> 日期：2026-11-14
> 节点：ID 4，IP `94.237.36.107`
> 站点：`https://www.linkcloud.network`
> 结果：**节点已上线并跑通真实流量；面板 UI 项（负载显示/在线 IP 页面）待人工确认**

## 1. 站点返回的节点配置

```json
{
  "port": 440,
  "stream_type": "tcp",
  "tls_type": "reality",
  "flow": "xtls-rprx-vision",
  "server_names": ["dlcdnets.asus.com"],
  "short_ids": ["<redacted>"],
  "dest": "dlcdnets.asus.com:443"
}
```

- `node_type`: `vless`
- `traffic_rate`: `1`
- pull/push interval: 60s
- node speed limit: 0

## 2. 数据规模

| 项 | 值 |
|---|---|
| `/users` | 4098（测试账户创建后 4099） |
| `/audit_rules` | 27 |
| `/white_list` | 0 |
| 规则编译 | 27/27，0 跳过 |

27 条规则全部是裸正则（如 `(.*\\.||)(dafahao|...)\\.(org|com|net)`）；`auto` 模式按 regex 编译成功。

## 3. 部署

- 交叉编译 `linux/amd64` 静态二进制，部署到 `/usr/local/bin/linkc-node`
- 配置 `/etc/linkc-node/config.yaml`（0600）
- 进程：`linkc-node run`，监听 `0.0.0.0:440`
- 状态目录：`/var/lib/linkc-node`
- 管理 socket：`/run/linkc-node/admin.sock`
- 当前进程 PID：1738；版本 0.8.0-m4
- 配置已开启 `traffic.include_online_zero=true`

## 4. 验证结果

| 项目 | 结果 | 证据 |
|---|---|---|
| REALITY + Vision 客户端连接 | ✅ | 真实 Xray 客户端经节点访问 `http://example.com`，HTTP 200 |
| 用户认证 | ✅ | 使用测试账户 ID 49720 成功 |
| 流量上报 | ✅ | 测试账户剩余流量从 1023.97MB → 1023.86MB（约 146KB）；`pending_bytes` 归零 |
| 状态上报 | ✅ | `last_success.status` 持续更新 |
| alive_ip | ✅ | 长连接期间 `online_ips=1`；`/alive_ip` POST 成功 |
| 面板在线数 | ✅ | 开启 `include_online_zero` 后，节点卡显示“1 在线” |
| 审计规则 | ✅ | 测试账户命中规则 23（`www.360.cn`），日志 `audit hit ... rule=23` |
| audit_log 上报 | ✅ | `last_success.audit_log` 在命中后更新 |
| 审计去重 | ✅ | `audit_hits=2`，`audit_reported=1` |
| 面板故障保持服务 | 未在真实站点演练 | mock 已覆盖 |
| `status.load` 前端显示 | ✅ | 用户/管理端确认显示正确 |
| Gatekeeper 指标 | ✅ | `/metrics` 暴露 limits/rejects/active |
| 用户限速 1 Mbps | ✅ | 512KiB loopback 3.48s（含 burst，平均 1.2Mbps） |
| 设备数 1 | ✅ | 第二源 IP 连接 EOF；`rejects_device=1`；释放后恢复 |
| 空 users | **待确认** | 需要后台把用户流量/节点带宽打到阈值 |
| 测试账户流量 | ✅ | 49720：1G 配额，联调后剩余 1023.86MB |
| `speed_limit` / `device_limit` | **待确认** | 需要设置低限额用户 |
| `checkNodeIp` | 可能关闭 | 从非节点 IP 直接 GET `/node` 成功 |
| 证书来源 | N/A | 当前为 REALITY |

## 5. 审计语义结论

- 站点现有 27 条规则全部是裸正则；
- 本程序 `rule_mode=auto` 将其按 regex 编译；
- 规则 23 成功匹配 `www.360.cn`；
- 因此对目标站点，`auto` 的行为与现有规则写作方式一致。
- 是否还需要 `keyword` 兼容，取决于后台是否有人写非正则的裸值（建议保持 `auto`，不要直接切 `regex` 全局强制）。

## 5.1 M3 限额验收

- 测试账户 49720 已限制为 `speed_limit=1`、`device_limit=1`。
- 在测试节点本机通过 loopback 目标测试，排除公网带宽瓶颈：
  - 512KiB 用时 3.48s，平均约 1.2Mbps；
  - token bucket 初始 burst 约 90KB，稳态约 1Mbps。
- 设备数测试：
  - 节点本机连接（源 127.0.0.1）保持时，从另一源 IP 连接返回 EOF；
  - 管理面 `rejects_device=1`；
  - 释放后重新连接成功。
- 测试期间临时开启 `freedom.allow_private_targets=true`，完成后已恢复 `false`。

## 6. 待人工确认

1. 在线 IP 页面是否有测试源 IP（连接保持期间）。
2. 是否要演练空 users / 限额 / 面板断网。
3. 测试账户 49720 的流量已产生约 146KB，剩余 1023.86MB。
4. 负载显示已由站点侧确认正确。

## 7. 复现命令

```bash
# 只读检查
curl -sS -H "API-KEY: <key>" -H "NODE-ID: 4" -H "NODE-TYPE: vless" \
  https://www.linkcloud.network/api/v1/node

# 本机管理面
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
```
