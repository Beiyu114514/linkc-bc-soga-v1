# linkc-node

[![CI](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/ci.yml/badge.svg)](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/ci.yml)
[![Docker](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/docker.yml/badge.svg)](https://github.com/Beiyu114514/linkc-bc-soga-v1/actions/workflows/docker.yml)
[![Release](https://img.shields.io/github/v/release/Beiyu114514/linkc-bc-soga-v1)](https://github.com/Beiyu114514/linkc-bc-soga-v1/releases)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](go.mod)

**linkc-node** 是一个自托管的 VLESS 节点代理，面向
**SSPanel-UIM + SogaController（soga-v1 WebAPI）** 部署场景。它将 Xray-core
内嵌到单个 Go 二进制中，提供用户同步、流量上报、在线 IP、审计、限额、热更新和本地管理接口。

> [!IMPORTANT]
> 项目仍处于 `0.x` 阶段。生产部署前请在目标系统完成升级、回滚和故障恢复演练，并遵守所在地法律法规及上游服务条款。

## 功能

- **面板兼容**：对接 SSPanel-UIM 的 soga-v1 节点接口，支持用户、路由、审计规则和流量同步。
- **单二进制部署**：内嵌 Xray-core，无额外运行时依赖，可作为 systemd 服务或容器运行。
- **协议覆盖**：VLESS over TCP/WS，支持 TLS、REALITY、Vision、XHTTP 和 gRPC 传输配置。
- **可靠同步**：ETag/304 缓存、请求重试、`last_good` 启动、流量 journal 和 at-least-once 上报。
- **运行时更新**：用户热添加/移除、同端口 inbound 切换、失败回滚和证书热加载。
- **治理能力**：全局/用户限速、设备数、TCP 连接限制、审计规则、白名单和在线 IP 上报。
- **本地管理面**：Unix socket 提供 `/healthz`、`/readyz`、`/status` 和 Prometheus `/metrics`。
- **多种部署方式**：一键脚本、deb/rpm、systemd、Docker Compose 兼容的容器命令。

## 快速开始

### 使用一键脚本

适合从源码仓库部署或升级 systemd 服务：

```bash
git clone https://github.com/Beiyu114514/linkc-bc-soga-v1.git
cd linkc-bc-soga-v1

printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-stdin
```

交互式部署：

```bash
sudo ./scripts/quickstart.sh wizard
```

只预览配置和命令，不写文件：

```bash
./scripts/quickstart.sh --dry-run \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-env PANEL_API_KEY
```

脚本会创建低权限 `linkc` 用户，安装二进制和 systemd unit，校验配置后启用并重启服务。
配置文件为 `root:linkc 0640`，服务用户只能读取，不能替换。

### 使用安装包

从 [最新 Release](https://github.com/Beiyu114514/linkc-bc-soga-v1/releases/latest)
下载对应架构的 deb 或 rpm：

```bash
sudo apt install ./linkc-node_0.12.0_amd64.deb
# RPM 系统改用：sudo rpm -Uvh linkc-node-0.12.0-1.x86_64.rpm

read -rsp 'Panel API key: ' PANEL_API_KEY; echo
printf '%s\n' "$PANEL_API_KEY" |
  sudo tee /etc/linkc-node/api-key >/dev/null
sudo chown linkc:linkc /etc/linkc-node/api-key
sudo chmod 0600 /etc/linkc-node/api-key

sudoedit /etc/linkc-node/config.yaml
sudo -u linkc linkc-node check -c /etc/linkc-node/config.yaml
sudo systemctl enable --now linkc-node
```

### 使用容器

```bash
docker pull ghcr.io/beiyu114514/linkc-bc-soga-v1:latest

sudo install -d -m 0750 -o root -g 10001 /etc/linkc-node
sudo install -d -m 0700 -o 10001 -g 10001 \
  /var/lib/linkc-node /var/lib/linkc-node/run

# 准备 /etc/linkc-node/config.yaml 与 /etc/linkc-node/api-key
# 文件需对 UID 10001 可读；更推荐使用 scripts/docker-launch.sh 自动生成
docker run --detach --name linkc-node --network host \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  -v /var/lib/linkc-node/run:/run/linkc-node \
  ghcr.io/beiyu114514/linkc-bc-soga-v1:latest
```

容器必须使用 host 网络，否则真实源 IP 和低端口绑定行为会受到影响。可进一步阅读
[Docker 命令行部署](docs/docker-launcher.md)，由其处理配置、密钥、证书和容器回滚。

## 配置

最小配置示例：

```yaml
panel:
  base_url: "https://panel.example.com/"
  profile: "sspanel-uim-soga"
  api_key_file: "/etc/linkc-node/api-key"
  node_id: "12"
  node_type: "vless"

node:
  listen: "0.0.0.0"

storage:
  path: "/var/lib/linkc-node"

admin:
  socket: "/run/linkc-node/admin.sock"
  metrics: true
```

完整字段、协议和限制说明见 [配置文档](docs/configuration.md)。

## 运维与排障

```bash
sudo systemctl status linkc-node
sudo journalctl -u linkc-node -f
sudo linkc-node check -c /etc/linkc-node/config.yaml
sudo linkc-node status -c /etc/linkc-node/config.yaml
curl --unix-socket /run/linkc-node/admin.sock http://localhost/readyz
```

常用文档：

- [部署与运维](docs/operations.md)
- [升级与回滚](docs/upgrade.md)
- [一键启动脚本](docs/quickstart.md)
- [Docker 命令行部署](docs/docker-launcher.md)
- [监控与告警](docs/monitoring.md)
- [常见问题](docs/faq.md)
- [安全说明](SECURITY.md)

## 构建与测试

需要 Go 1.27 或更高版本。

```bash
make build
make test
make race
make vet
make test-launcher
make test-quickstart
```

生成发布资产：

```bash
make release-artifacts VERSION=0.12.0
```

发布流程和产物说明见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 兼容性

| 项目 | 支持范围 |
|---|---|
| Linux | amd64、arm64 |
| systemd | 支持 `Type=simple` 服务与低端口 capability |
| 容器 | `linux/amd64`、`linux/arm64` |
| 面板 | SSPanel-UIM + SogaController（soga-v1） |
| Go | `go.mod` 中声明的版本或更新版本 |

Windows、macOS、Windows 容器和非 systemd 发行版未作为发布目标验证。

## 安全

- 不要提交或公开 API Key、节点 UUID、REALITY 私钥、证书私钥和生产配置。
- 生产环境保持 `node.freedom.allow_private_targets: false`，避免 SSRF。
- 安全漏洞请通过 [GitHub Security Advisory](https://github.com/Beiyu114514/linkc-bc-soga-v1/security/advisories/new)
  私下报告，不要在公开 issue 中披露利用细节。

## 参与贡献

提交代码前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。参与项目即表示同意遵守
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。

## 项目状态与边界

- 这是第三方、独立维护的项目，与 Xray-core、SSPanel-UIM 或 Soga 的维护者没有隶属关系。
- 本项目只提供节点侧代理与管理能力，不提供面板、订阅页、支付或用户运营系统。
- 目标站点源码、站点数据库和用户凭据不属于本仓库，也不应提交到本仓库。
- 历史设计和验收记录保留在 `M0-REPORT.md` 至 `M5-REPORT.md`、`PLAN.md` 与
  [发布记录](CHANGELOG.md)中，仅用于开发追溯，不代表当前版本的兼容性承诺。

## 许可证

本仓库当前采用“保留所有权利”的源代码可见许可，详见 [LICENSE](LICENSE)。
第三方依赖仍受各自许可证约束，详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
