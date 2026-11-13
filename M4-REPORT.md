# M4 进展报告：加固与交付

> 对应规划：`PLAN.md` §15 M4
> 日期：2026-11-14
> 状态：**工程化能力完成，长稳/混沌与完整回滚待补**

---

## 1. 本轮完成

### 1.1 inbound 切换与回滚

- 修复热切换 `oldTag` 读取顺序 bug：
  - `oldTag` 必须在 `AddInbound` 之前读取；
  - 之前误删新 inbound，后续 `ApplyUsers` 报 `handler not found`。
- 新增回归测试 `TestInboundSwapThenUserApply`。
- 新增 `TestInboundApplyFailureKeepsOld`：
  - 面板返回无证书的 `tls` 配置时，旧 inbound 继续服务。
- 当前语义：
  - 新 inbound 添加失败 → 保留旧 inbound；
  - 新 inbound 成功、旧 inbound 移除失败 → 记录告警，新 inbound 继续服务；
  - 尚未实现“移除旧失败后回滚到旧配置”的完整事务。

### 1.2 证书 / SIGHUP / 多证书

- `node.tls.certificates` 支持多组证书，Xray 按 SNI 选择。
- 证书文件读取为 inline PEM，避免 Xray 文件热加载的上游 data race。
- 新增 `Agent.ReloadCerts()`：
  - 重新读取证书文件；
  - 证书变化时新建 inbound → 移除旧 inbound；
  - 失败保留旧 inbound；
  - CLI 捕获 SIGHUP 调用。
- `TestBuildConfigMultipleCertificates` 覆盖多证书配置生成。
- 真实 TLS 证书轮换的 SIGHUP E2E 仍待补。

### 1.3 安全加固

- CLI 启动设置 `umask 077`。
- systemd：
  - `UMask=0077`
  - `NoNewPrivileges`
  - `ProtectSystem=strict`
  - `ProtectHome`
  - `ProtectClock/Hostname`
  - `RestrictSUIDSGID/Realtime`
  - `SystemCallArchitectures=native`
  - 仅 `CAP_NET_BIND_SERVICE`
- 面板 HTTPS 严格校验、禁止重定向、API key 不落日志。
- `dump-config` 脱敏 UUID/私钥/证书。
- `freedom.allow_private_targets` 默认 false。
- Docker 非 root（UID 10001），推荐 `network_mode: host` + 只读根文件系统。

### 1.4 交付物

- `Makefile`：
  - `build` / `linux-amd64` / `linux-arm64`
  - `test` / `race` / `vet` / `package-deb` / `package-rpm`
- `scripts/install.sh`：用户、目录、二进制、systemd unit、示例配置。
- `packaging/nfpm.yaml`：deb/rpm 模板（需安装 nfpm）。
- `docs/upgrade.md`：升级/回滚/证书轮换。
- `docs/faq.md`：离线、负载、审计、流量重复等常见问题。
- `docs/operations.md`：安装、SIGHUP、多证书、管理面。

---

## 2. 测试

```bash
go test ./... -count=1
go test -race ./internal/agent ./internal/audit ./internal/xray -count=1
go vet ./...
```

新增覆盖：

| 测试 | 结果 |
|---|---|
| `TestInboundSwapThenUserApply` | ✅ inbound 切换后继续增删用户 |
| `TestInboundApplyFailureKeepsOld` | ✅ 非法配置保留旧 inbound |
| `TestBuildConfigMultipleCertificates` | ✅ 多证书配置 |
| `TestStartXHTTPAndGRPC` | ✅ 协议回归 |
| `TestDeviceIPv6Aggregation` | ✅ 限额回归 |
| `TestStartUserSpeedLimit` | ✅ 限速回归 |

真实站点：

- 节点 4 使用 `0.7.3-m3` 验证通过；
- M4 二进制已升级到 `0.8.0-m4`（PID 1738）；
- REALITY 配置下 SIGHUP 只做证书空操作（无 TLS 证书），不触发错误。

---

## 3. 尚未完成

1. **完整 inbound 回滚**：旧 inbound 移除失败后的自动回滚。
2. **SIGHUP 证书轮换 E2E**：真实 TLS 证书替换 + 客户端验证。
3. **多证书/SNI 真实场景**：需要多个域名证书。
4. **混沌/长稳**：随机断网、面板 5xx/429、磁盘满、核心崩溃恢复、24h+ 长稳。
5. **打包实测**：nfpm deb/rpm 产物安装/升级/回滚。
6. **安全审计**：systemd 沙箱在真实发行版上的兼容性、低端口、SELinux/AppArmor。
7. **压测**：10k 用户、并发连接、节点全局/用户限速精度。
8. **Prometheus 告警模板**。

---

## 4. 验收建议

M4 目前达到“可部署、可运维、可回滚（数据/二进制）”的工程化水平；建议在灰度前完成：

1. 真实 TLS 证书 SIGHUP 轮换；
2. deb/rpm 实际安装/升级/回滚；
3. 面板断网/429/磁盘满故障注入；
4. 24h 长稳；
5. 低限额/多设备最终验收。

之后可进入灰度发布。
