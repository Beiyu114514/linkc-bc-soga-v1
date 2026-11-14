# M5 进展报告：收尾（回滚事务 / 证书 E2E / 混沌 / 压测 / 打包 / 告警）

> 对应规划：`PLAN.md` §15 M4 遗留项、§12 可观测性、§14 测试计划
> 日期：2026-11-14
> 状态：**除 24h 长稳实跑与目标机兼容性验证外，M4 遗留全部关闭**

---

## 1. inbound 热切换：事务化 + 自动回滚

`internal/xray/runtime.go` 新增 `Engine.SwapInbound(ctx, node, users) (SwapReport, error)`，
替代原先 agent 里的 add/remove 两步操作。语义：

| 场景 | 行为 |
|---|---|
| 新 inbound 添加失败 | 旧 inbound 原样服务，`Tag` = 旧 tag，agent 下一轮重试 |
| 添加成功、旧移除成功 | 提交新状态（tag/users/port/gate 限额） |
| 旧移除失败 | **回滚**：移除新 inbound；旧 handler 仍在册则直接恢复引擎状态，否则用旧配置重加（旧 tag） |
| 回滚时新 inbound 也摘不掉 | 保留新 inbound 服务，`Warning` 报告残留旧监听 |
| 回滚本身失败（旧配置无法恢复） | 尝试把新配置加回来；仍失败则 `Tag=""`，agent 置空 `inboundFP` 强制下轮重放 |

实现要点：Xray 的 `core.AddInboundHandler` 从实例自身取 manager（包装 `Engine.inboundMgr`
拦不住 add），因此 add 失败经 `addInbound` 间接层注入；移除失败经 manager 包装注入。
`RemoveHandler` 对已注册 tag 实际几乎不失败（close 错误仅记日志），失败主要来自
tag 不存在/管理器异常，回滚逻辑按此现实设计。

测试（`internal/xray/swap_test.go`，5 条分支全覆盖）：
`TestSwapInboundRollbackRestoresOld`（回滚后旧端口/旧用户集仍在服务，新端口关闭，
随后无故障 swap 自愈）、`TestSwapInboundAddFailureKeepsOld`、
`TestSwapInboundKeepsNewWhenOldRemovalStuck`、`TestSwapInboundRollbackFailureKeepsNew`、
`TestSwapInboundNoListenerIsRecoverable`。

agent 侧（`internal/agent/sync.go`、`agent.go ReloadCerts`）改为按 `SwapReport`
提交/回滚/强制重放，并抽出 `commitNode`。

## 2. 证书：真实轮换 E2E + 多证书 SNI + 一个真 bug

- `internal/agent/certs_test.go`：
  - `TestReloadCertsRotatesServingCertificate`：TLS 节点 + 本地证书文件启动 →
    客户端按 pin A 连通 → 磁盘替换证书 → `ReloadCerts()`（SIGHUP 路径）→
    按 pin B 连通、pin A 握手失败 → 空 reload 为 no-op。
  - `TestReloadCertsMissingFileKeepsOld`：key 文件损坏时 reload 报错且旧 inbound 继续服务。
- `internal/xray/certs_test.go` `TestStartMultiCertificateSNI`：一个 inbound 两张证书，
  按 SNI 选 cert（a.test→A、b.test→B），错 pin 必须失败。
- **修复 bug**：`node.Spec.Xray()` 未透传 `LocalRuntime.Certificates`，
  导致 `node.tls.certificates`（M4 多证书功能）在 agent 路径被静默丢弃——
  M4 只测了 `BuildConfig` 层所以没暴露。回归测试
  `TestSpecPassesLocalCertificates`。

## 3. 混沌测试（`internal/agent/chaos_test.go`）

mock panel 新增故障注入 `SetFault(path, status, times)` / `ClearFaults()`
（`internal/m0/mockpanel/panel.go`）。

| 测试 | 验证 |
|---|---|
| `TestPanelGetServerErrorsKeepServing` | GET /node 持续 500：数据面不停、新会话可用、degraded、恢复后自愈 |
| `TestPanelWebAPIDisabledKeepsServing` | webapi 503：同上 |
| `TestPanelPostThrottleKeepsTrafficJournal` | POST /traffic 持续 429：journal 积压有界，恢复后追平且面板收到流量 |
| `TestTrafficJournalFullPausesCollection` | `max_journal_bytes` 触顶：采集暂停、积压冻结、数据面与新会话不受影响 |

## 4. 压测与 benchmark（`internal/xray/load_test.go`）

本机（i3-12100T，loopback）实测：

| 项 | 结果 |
|---|---|
| `ApplyUsers(10000)` | 19.4ms（benchmark 4.79ms/op，热路径） |
| `ReadTraffic(10000 users)` | 1.85ms（benchmark 2.16ms/op） |
| `OnlineIPs(10000 users)` | 0.70ms |
| 数据面吞吐（limits off，单连接 64MiB） | 22.3Gbps（67108864B / 24.1ms） |
| 10k 用户后真实客户端认证 | ✅（user 10000 连通） |

运行：`go test ./internal/xray -run 'TestEngine' -v`、`make bench`。

## 5. 长稳 harness（`internal/agent/soak_test.go`）

`SOAK_DURATION=<dur> go test ./internal/agent -run TestSoak`（或 `make soak`）。
每轮随机注入：面板 5xx/429、webapi 503、**节点配置端口切换**（走新 SwapInbound）、
用户 churn、**证书轮换**、流量突发；每轮后断言数据面恢复、`ready`、journal 有界、
agent 不退出。已跑 3 个种子 ×60s 与 25s 冒烟通过（约 20 轮/秒）。
**24h 实跑仍未做**（需在目标机执行 `SOAK_DURATION=24h make soak`）。

## 6. 打包实测

- `nfpm` 打包 deb/rpm 成功；deb 实测 **安装 → 升级覆盖（0.9.0→0.10.0）→ purge**，
  文件与权限符合预期（bin 0755、config 0600 conffile、state 目录 0700）；
  rpm 已用 `rpm -qp` 核对文件清单与权限（本机为 deb 系，未做 rpm live install）。
- `linkc-node check/version` 子命令冒烟通过。

## 7. 可观测性

- 新增指标 `linkc_audit_pending`（`store.PendingAuditCount` → `audit.Checker.Pending`
  → Snapshot → `/metrics`），补齐 PLAN §12.2 缺口。
- `deploy/alerts.yml`：7 条告警（NotReady/Degraded/TrafficBacklog/AuditBacklog/
  NoUsers/LimitRejectsSpike/AuditBlockedSpike）。
- `docs/monitoring.md`：指标清单 + Unix socket 抓取方案（textfile collector / socat）。
- `docs/security-audit.md`：systemd 沙箱、凭据、信任边界、供应链审计与遗留（S1/S2）。

## 8. 尚未完成

1. **24h 长稳实跑**（harness 就绪）：`SOAK_DURATION=24h make soak`。
2. **多域名真实证书场景**：已用两张自签证书（a.test/b.test）验证 SNI 选择；
   真实 CA 多域名证书需上线环境验证。
3. **systemd 沙箱 / SELinux / AppArmor 目标机兼容性**（`docs/security-audit.md` S1/S2）。
4. **ECH 实际握手**：站点不下发 ECH 且缺测试 key set（M0 起的历史遗留）。
5. **多出口 IPv6 压测**：设备数 /64 聚合有单测与真实站点 1 设备验收，多出口未测。
6. **站点侧 P0 待确认**：见 PLAN §17（部署版本一致性、在线数口径、xtls 映射、
   checkNodeIp、证书来源、传输范围）。

## 9. 变更文件

```
internal/xray/runtime.go        SwapInbound 事务 + commit/userListLocked + addInbound seam
internal/xray/swap_test.go      新增：5 条回滚分支测试
internal/xray/certs_test.go     新增：多证书 SNI E2E
internal/xray/load_test.go      新增：10k 用户/吞吐测试 + 2 个 benchmark
internal/agent/sync.go          SwapInbound 集成（提交/回滚/强制重放）
internal/agent/agent.go         ReloadCerts 事务化 + commitNode + AuditPending
internal/agent/certs_test.go    新增：SIGHUP 轮换 E2E、坏 key 文件
internal/agent/chaos_test.go    新增：4 个混沌测试
internal/agent/soak_test.go     新增：长稳 harness
internal/node/spec.go           修复：透传 local.Certificates
internal/node/node_test.go      新增：多证书透传回归
internal/audit/pusher.go        新增：Checker.Pending()
internal/store/store.go         新增：PendingAuditCount()
internal/admin/admin.go         新增：linkc_audit_pending 指标
internal/m0/mockpanel/panel.go  新增：SetFault/ClearFaults 故障注入
Makefile                        新增 soak / bench 目标
deploy/alerts.yml               新增：Prometheus 告警规则
docs/monitoring.md              新增：指标/抓取/告警说明
docs/security-audit.md          新增：安全审计
PLAN.md                         M0–M4 状态、P0、变更记录 v0.9
```

## 10. 验证命令

```bash
make vet && make test && make race
make bench
SOAK_DURATION=60s SOAK_SEED=7 make soak
make linux-amd64 VERSION=0.9.0 && make package-deb package-rpm VERSION=0.9.0
```
