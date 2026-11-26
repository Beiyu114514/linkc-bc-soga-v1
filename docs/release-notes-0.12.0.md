# linkc-node 0.12.0

`0.12.0` 聚焦部署安全、运维一致性和核心稳定性。这是 `0.11.1` 之后的次版本更新。

## 重点更新

### 一键启动与安全加固

- 新增 `scripts/quickstart.sh`，支持 systemd、Docker、前台、向导、dry-run 和
  stage-only 模式。
- 配置路径覆盖必须绝对、规范、彼此独立，并使用 `linkc-node` 专用目录。
- 配置、API Key 和白名单文件拒绝符号链接，并使用原子替换。
- API Key 通过标准输入或环境变量传递，不进入 sudo 命令行。
- 向导输入会在安装前重新校验；`--wait` 只接受非负整数。
- systemd 重装或升级后会执行 `daemon-reload`、`enable` 和 `restart`。

配置权限统一为：

```text
/etc/linkc-node                  root:linkc 0750
/etc/linkc-node/config.yaml      root:linkc 0640
/etc/linkc-node/api-key          linkc:linkc 0600
/var/lib/linkc-node              linkc:linkc 0700
/run/linkc-node                  linkc:linkc 0700
```

### CLI 部署管理

- Docker 部署入口改为纯命令行，移除内置 Web 页面。
- `wizard`、`deploy`、`preview`、`status`、`node-status`、`logs` 和
  `node-ready` 提供脚本化部署与排障流程。
- 容器状态包含 health 信息，日志 JSON 支持有上限的 tail。
- 命令行选项与配置校验规则保持一致。

### 核心稳定性

- 改进 Xray 运行时 inbound 切换、连接统计、限额并发和审计 geodata 处理。
- 管理命令未显式指定 socket 时加载配置；配置缺失或损坏会给出明确错误。
- 改进面板请求重试、用户热更新、流量 journal、在线 IP 和证书轮换路径。
- 修复 deb/rpm 安装用户、目录权限、卸载和升级重启流程。

## 破坏性变更

- Docker launcher 不再启动本地 Web 页面；现有使用方式需改为命令行或
  `wizard`。
- quickstart 和安装包生成的配置目录改为 `root:linkc`，配置文件不再由
  `linkc` 用户持有。请使用 root 编辑，并通过 `systemctl restart` 应用。
- 使用 `--api-key-env` 时不要再在外层包 `sudo`，否则普通 sudo 会清除变量。

## 升级

源码部署：

```bash
git pull --ff-only
printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-stdin
```

安装包：

```bash
sudo apt install ./linkc-node_0.12.0_amd64.deb
# 或
sudo rpm -Uvh linkc-node-0.12.0-1.x86_64.rpm
sudo systemctl status linkc-node
```

Docker：

```bash
docker pull ghcr.io/beiyu114514/linkc-bc-soga-v1:0.12.0
```

升级前请备份 `/var/lib/linkc-node/state.db` 和现有配置。更完整的回滚步骤见
[upgrade.md](./upgrade.md)。

## 发布资产

- `linkc-node-linux-amd64`
- `linkc-node-linux-arm64`
- amd64/arm64 deb 和 rpm
- `LICENSE`
- `THIRD_PARTY_NOTICES.md`
- `SHA256SUMS`

下载后请使用 `sha256sum -c SHA256SUMS` 校验。

## 已知限制

- Windows、macOS 和非 systemd 发行版未作为发布目标验证。
- ECH 实际握手仍缺少完整的外部测试凭据。
- 多出口 IPv6 和 24 小时长稳环境仍需由部署方按实际网络验收。
