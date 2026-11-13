# M2 进展报告：审计 / alive_ip / 在线数

> 对应规划：`PLAN.md` §15 M2
> 日期：2026-11-14
> 状态：**M2 核心完成**（8 接口 + audit/alive_ip + XHTTP/GRPC + ECH/decryption 配置）
> 真实站点记录见 `LIVE-VALIDATION.md`；负载显示已确认

---

## 1. 本轮完成

### 1.0 协议完整性（新增）

- XHTTP：`xhttp_settings`（host/path/mode/headers）映射；本地 `node.xhttp` 覆盖；实际 TLS 传输 E2E 通过。
- gRPC：`grpc_settings`（authority/serviceName/multiMode/timeouts/window/userAgent）；本地 `node.grpc` 覆盖；实际 TLS 传输 E2E 通过。
- ECH：面板 `ech_key` 或本地 `node.tls.ech_server_keys`，映射到 `tlsSettings.echServerKeys`；仅 TLS。
- decryption：面板 `decryption` 或本地 `node.vless.decryption`；默认 `none`。
- 目标站点当前仍未下发这些字段，因此真实站点继续使用面板的 REALITY 配置。

### 1.1 审计规则（`audit_rules` / `white_list`）

- Panel Client 新增：
  - `GET /api/v1/audit_rules`（ETag/304）
  - `GET /api/v1/white_list`（ETag/304）
  - `POST /api/v1/audit_log`
- 新增 `internal/audit`：
  - 规则前缀：`regexp:` / `domain:` / `full:` / `keyword:` / `ip:` / `port:`
  - 裸值 `auto`：含明确正则元字符（`\ * + ? [ ] ( ) { } | ^ $`）按 regex，否则 keyword
  - 白名单优先于审计规则
  - 第一条命中即生效（面板顺序）
  - 按 `(user, rule)` + `dedupe_ttl` 本地去重；bbolt 持久化
  - 事件写入 `audit_queue`，推送成功后才 ack；站点侧自然日去重
  - 规则热更新：编译成功才替换，失败保留旧规则
  - 30s 匹配缓存，规则更新后自然失效
- Gatekeeper 在出站入口执行审计：
  - 使用 sniffing 后的 `session.Outbounds.Target`
  - `action=block`：不拨号，直接断开
  - `action=report`：放行，仅上报
  - 支持白名单跳过
  - 审计与限额模式解耦：`limits.mode=off` 时审计仍生效

### 1.2 alive_ip

- 新增 `internal/agent/alive.go`：
  - `changed`：仅推送变化的用户 IP
  - `full`：每次全量
  - `changed` 模式每 N 次（默认 20）补一次全量
  - 目标站点不清除离线 IP，因此不发送空列表
- 每 `push_interval` 推送 `/api/v1/alive_ip`。

### 1.3 在线数

- `traffic.include_online_zero=true`：
  - 在流量批次之后补发一条仅含在线用户的 `0/0` 请求
  - 站点 `online_user=count(payload)` 得到完整在线数
  - 代价：每轮写 0/0 `user_traffic_log`

### 1.4 指标

管理面新增：

- `linkc_audit_rules`
- `linkc_audit_hits_total`
- `linkc_audit_reported_total`
- `linkc_audit_blocked_total`

`/status` 同步包含 `audit_rules`、`audit_hits`、`audit_reported`、`audit_blocked`。

---

## 2. 测试

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

新增 E2E：

| 测试 | 结果 |
|---|---|
| `TestAuditBlocksAndReports` | ✅ 规则命中阻断 + audit_log + 去重 |
| `TestAuditReportModeAllows` | ✅ report 放行但上报 |
| `TestAuditDomainBlock` | ✅ 域名 sniffing 后命中 |
| `TestStartXHTTPAndGRPC` | ✅ XHTTP + TLS、gRPC + TLS 实际连接 |
| `TestBuildConfigExtendedTransport` | ✅ ECH / decryption / XHTTP / gRPC 映射 |
| `TestWhitelistSkipsAudit` | ✅ 白名单不阻断、不上报 |
| `TestAliveIPPushed` | ✅ 非 loopback 源 IP 出现在 alive_ip |
| 审计/白名单 panel 契约 | ✅ |
| 全量 `-race` | ✅ |

---

## 3. 尚未完成 / 可选

1. **ECH 实际握手**：配置映射已实现，但缺少合法测试 ECH key set 做端到端握手；站点不下发。
2. **告警**：指标已暴露，Prometheus 规则/告警模板仍需补。
3. **崩溃恢复细化**：审计队列已持久化；启动时优先补推审计事件可继续优化。
4. **真实站点**：`status.load` 已确认；空 users/限额/断网演练仍待后台配合。

---

## 4. 配置

```yaml
node:
  audit:
    enabled: true
    action: "report"        # 先 report，确认无漏判后改 block
    rule_mode: "auto"
    whitelist_file: "/etc/linkc/whitelist.txt"
    dedupe_ttl: 24h
    strict_rules: false
  alive_ip:
    mode: "changed"
    full_every: 20
  traffic:
    include_online_zero: false
```

---

## 5. 真实站点结果

- 节点 ID 4，REALITY + Vision，真实客户端 HTTP 200；
- 4098 用户（测试账户后 4099）、27 条裸正则审计规则全部编译；
- 测试账户 49720 跑通真实流量；剩余流量 1023.97MB → 1023.86MB；
- 规则 23 命中 `www.360.cn`，`audit_log` 推送成功且本地去重生效；
- 开启 `include_online_zero` 后，面板节点卡显示“1 在线”；
- traffic/status/alive_ip 均被站点接受；`pending_bytes` 归零；
- `status.load` 已由站点侧确认显示正确。

## 6. 下一步

1. 用真实测试节点验证一条裸 regex 规则（`.*google\.com`）在 soga 侧的行为，再决定生产 `rule_mode`。联调步骤见 **`docs/p0-live-validation.md`**。
2. 完成 XHTTP/GRPC 本地扩展参数与 ECH/decryption（或明确标记为不支持）。
3. 接入 geosite/geoip 与域名解析，补齐 M3 的审计执行。
4. Prometheus 告警模板、长稳/压测。
