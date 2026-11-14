# linkc-bc-soga-v1

[![ci](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/ci.yml/badge.svg)](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/ci.yml)
[![docker](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/docker.yml/badge.svg)](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/docker.yml)

基于 **Xray-core** 的 soga-v1 节点后端（soga 替代品）。

- **目标站点**：SSPanel-UIM + `SogaController`（soga-v1 WebAPI 服务端）
- **本程序**：节点侧客户端，内嵌 Xray-core，运行 VLESS 节点
- **当前阶段**：**M5 收尾完成**；等待 24h 长稳实跑与目标机（systemd/SELinux）兼容性验收

## 文档

- [docs/usage.md](./docs/usage.md)：**使用文档**（安装 → 配置 → 启动验证 → 常用操作 → 排障）
- [PLAN.md](./PLAN.md)：总体技术规划（v0.9）
- [M0-REPORT.md](./M0-REPORT.md)：技术验证结果
- [M1-REPORT.md](./M1-REPORT.md)：MVP 实现、测试与遗留
- [M2-REPORT.md](./M2-REPORT.md)：审计/alive_ip/协议完整性与剩余项
- [M3-REPORT.md](./M3-REPORT.md)：限额/审计执行与压测遗留
- [M4-REPORT.md](./M4-REPORT.md)：加固、证书/SIGHUP、打包与验收遗留
- [M5-REPORT.md](./M5-REPORT.md)：收尾：回滚事务/证书 E2E/混沌/压测/打包/告警
- [LIVE-VALIDATION.md](./LIVE-VALIDATION.md)：真实站点联调记录
- [docs/configuration.md](./docs/configuration.md)：配置说明
- [docs/docker-launcher.md](./docs/docker-launcher.md)：Docker 一键启动（图形化参数页）
- [docs/operations.md](./docs/operations.md)：部署/运维
- [docs/monitoring.md](./docs/monitoring.md)：指标、抓取与告警（`deploy/alerts.yml`）
- [docs/security-audit.md](./docs/security-audit.md)：安全审计
- [docs/p0-live-validation.md](./docs/p0-live-validation.md)：真实站点联调清单

## 已实现（M1）

- 内嵌 Xray-core，VLESS + TCP / WS + TLS / REALITY；Vision 已验证
- soga-v1 客户端：`/node`、`/users`、`/traffic`、`/status`
- ETag/304 持久化缓存、GET 重试/退避
- 用户热添加/移除、UUID 冲突检测、空用户列表、批量删除安全阀
- 审计规则/白名单拉取、regex/keyword/ip/port 匹配、report/block、去重队列
- `alive_ip` changed/full 推送；`traffic.include_online_zero`
- XHTTP/GRPC 实际 TLS 传输；ECH/decryption 配置映射
- 节点全局限速、用户限速、设备数（IPv6 /64）、TCP 限制
- geosite/geoip 审计、域名解析 IP 规则
- 多证书/SNI、SIGHUP 证书热加载、inbound 切换失败保留旧配置
- inbound 热切换事务化：旧移除失败自动回滚到上一份配置（`Engine.SwapInbound`）
- 同端口 inbound 热切换（`SO_REUSEPORT`）
- 流量 `Set(0)` + bbolt journal + 批量推送/ack（at-least-once）
- `last_good` 启动、面板故障保持服务、崩溃恢复 journal
- 状态采样：`load` CPU 小数 + cpu/mem/swap/disk/uptime
- CLI：`run` / `check` / `dump-config` / `version`
- Unix socket 管理面：`/healthz`、`/readyz`、`/status`、`/metrics`
- systemd unit、Dockerfile

## 构建

需要 Go 1.27+：

```bash
export PATH=/usr/local/go/bin:$PATH
go build -trimpath -o linkc-node ./cmd/linkc-node
```

## 发布

- 打 tag `vX.Y.Z` 会触发 `.github/workflows/release.yml`：构建 amd64/arm64 二进制、deb/rpm 与 `SHA256SUMS`，并创建 GitHub Release。
- `.github/workflows/docker.yml` 构建多架构镜像并推送到 `ghcr.io/beiyu114514/linkc-bc-soga-v1`：
  标签 → `X.Y.Z` / `X.Y` / `latest`，`main` → `edge`，另附 `sha-<short>`。
- 本地复现发布产物：`make release-artifacts VERSION=0.10.0`。
- 手动重建某个 tag：`gh workflow run release.yml -f tag=v0.10.0`。

Xray 版本固定为 release `v26.11.9` 的 commit 伪版本：
`v1.260327.1-0.20261109052709-676eaf66f8f0`
（GitHub tag `v26.11.9` 不是合法 Go module semver）。

## Docker 一键启动（图形化）

```bash
# 默认监听 0.0.0.0；自动生成并打印页面密码
sudo ./scripts/docker-launch.sh --username admin

# 仅本机访问
sudo ./scripts/docker-launch.sh --bind 127.0.0.1

# 用哈希文件（不保存明文）
sudo ./scripts/docker-launch.sh --hash-password --hash-password-output /root/.linkc-node/password.hash
sudo ./scripts/docker-launch.sh --username admin --password-hash-file /root/.linkc-node/password.hash
```

脚本会在本机启动一个带随机 token 的图形化参数页，填写面板地址、API Key、节点 ID 后自动生成
`config.yaml` / `api-key` / 证书 / `docker-run.sh` 并启动容器。默认使用 host 网络，页面监听
`0.0.0.0`；**监听 `0.0.0.0` 时必须加 `--username` 启用登录**，否则启动器拒绝启动；仅本机用 `--bind 127.0.0.1`。
加 `--username` 会自动生成随机密码并打印，同时写入 0600 JSON 凭据文件（默认
`~/.linkc-node/launcher-credentials.json`）；`--ask-password` 可手动输入，`--password-hash-file`
可读取 scrypt 哈希文件（不保存明文）。非 root 运行时使用
`~/.linkc-node`。详细说明见 [docs/docker-launcher.md](./docs/docker-launcher.md)。

## 快速检查 / 运行

```bash
PANEL_API_KEY=... ./linkc-node check -c configs/linkc-node.example.yaml
PANEL_API_KEY=... ./linkc-node dump-config -c configs/linkc-node.example.yaml
PANEL_API_KEY=... ./linkc-node run -c configs/linkc-node.example.yaml
```

管理面：

```bash
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
```

## 测试

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
# 随机故障注入长稳（默认跳过；SOAK_DURATION=24h 跑全天候）
SOAK_DURATION=60s go test ./internal/agent -run TestSoak -count=1
# 基准
go test ./internal/xray -run '^$' -bench . -benchtime 20x
```

测试覆盖：mock panel 契约、ETag/重试、bbolt journal、用户热更新、空 users、同端口切换、
TCP/WS+TLS/REALITY/XHTTP/GRPC、审计命中、alive_ip、面板故障保持服务、last_good 重启、
inbound 回滚事务（5 分支）、证书轮换/多证书 SNI、面板 5xx/429/journal 写满混沌、10k 用户与吞吐。

## 尚未实现

- 24h 长稳实跑（harness 已就绪：`make soak`）
- systemd 沙箱/SELinux/AppArmor 目标机兼容性（见 `docs/security-audit.md` S1/S2）
- ECH 实际握手测试（缺合法测试 key set）
- 多出口 IPv6 压测；站点侧 P0 待确认项（见 PLAN §17）

## 注意

- 目标站点源码归档包含站点配置与 `.git`，请按敏感资料保管，不要提交到公开仓库。
- 生产环境默认 `freedom.allow_private_targets=false`，防止 SSRF。
- 面板 API key、UUID、REALITY 私钥不要提交到仓库。
