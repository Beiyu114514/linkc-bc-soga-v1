# 使用文档

从零把 `linkc-node` 接到一个 SSPanel-UIM + `SogaController` 站点上，并完成日常运维。
更细的字段说明见 [configuration.md](./configuration.md)，部署细节见 [operations.md](./operations.md)。

> 只想尽快跑起来：用一键启动脚本 `sudo ./scripts/quickstart.sh wizard`，
> 它会完成第 2–4 节的大部分步骤，详见 [quickstart.md](./quickstart.md)。

## 1. 前置条件

- 一台 Linux 节点机（amd64 / arm64），能访问面板的 `base_url`；
- 面板侧已创建 VLESS 节点，拿到 `node_id` 与 `API-KEY`；
- 节点端口在面板上已配置，防火墙/安全组放行；
- 使用 TLS 时准备好证书文件；使用 REALITY 时参数由面板下发，无需本地证书。

程序内嵌 Xray-core，不需要单独安装或管理 Xray 进程。

## 2. 安装

### 2.0 一键启动（推荐）

`scripts/quickstart.sh` 把「生成配置 → 本地校验 → 安装 systemd 服务 → 启动 → 等就绪」串成一条命令：

```bash
# 非交互（API Key 从标准输入，不会出现在 ps 中）
printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-stdin

# 交互式向导 / 只看要做什么
sudo ./scripts/quickstart.sh wizard
./scripts/quickstart.sh --dry-run --panel-url https://panel.example.com --node-id 12 --api-key-env PANEL_API_KEY
```

它会写入 `/etc/linkc-node/config.yaml`（最小可用配置，`root:linkc` 0640）与
`/etc/linkc-node/api-key`（`linkc:linkc` 0600），
复用 `scripts/install.sh` 安装 `linkc` 用户、二进制与 unit，`check` 通过后
启用并显式重启服务，最后轮询 `readyz` 并打印 `status`；未就绪时附带最近日志。

常用开关：`--mode docker`（转调 `scripts/docker-launch.sh deploy`）、`--mode foreground`（前台调试）、
`--cert-file/--key-file`（TLS 证书对）、`--audit-action block`、`--stage-only`（只生成配置）、
`--force`（覆盖已有配置）、`--wait SECONDS`（等待就绪的秒数）。完整参数见
[quickstart.md](./quickstart.md) 与 `./scripts/quickstart.sh --help`。

### 2.1 二进制 / 安装包

```bash
# 从 GitHub Release 下载对应架构的二进制或 deb/rpm
sudo dpkg -i linkc-node_<version>_amd64.deb     # 或 rpm -i
# 或手动放置
sudo install -m 0755 linkc-node /usr/local/bin/linkc-node
```

deb/rpm 安装包的 postinstall 会创建 `linkc` 用户并修正配置/状态目录权限；仓库内安装脚本也会创建用户、目录与 systemd unit：

```bash
make build
sudo ./scripts/install.sh
```

`scripts/install.sh` 只负责安装，不启动服务；配置需自行准备，然后
`systemctl enable linkc-node && systemctl restart linkc-node`。

### 2.2 Docker

```bash
docker run -d --name linkc-node --network host --restart unless-stopped \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  ghcr.io/beiyu114514/linkc-bc-soga-v1:latest
```

必须用 `--network host`，否则丢失真实源 IP，`alive_ip` 与设备数限制会失真。

也可以用启动器一键生成配置并部署（支持交互向导、证书暂存、备份与回滚）：

```bash
sudo ./scripts/docker-launch.sh wizard
# 或 ./scripts/quickstart.sh --mode docker --panel-url https://panel.example.com --node-id 12 --api-key-env PANEL_API_KEY
```

详见 [docker-launcher.md](./docker-launcher.md)。

## 3. 配置

配置文件默认 `/etc/linkc-node/config.yaml`，权限 `0640 root:linkc`，
目录权限 `0750 root:linkc`；API Key 文件为 `0600 linkc:linkc`。
完整示例见 [`configs/linkc-node.example.yaml`](../configs/linkc-node.example.yaml)。

最小可用配置：

```yaml
panel:
  base_url: "https://panel.example.com/"   # 必须与站点授权路径一致
  api_key_file: "/etc/linkc-node/api-key"  # 推荐；也可用 api_key: "${PANEL_API_KEY}"
  node_id: "12"
  node_type: "vless"

node:
  listen: "0.0.0.0"
  tls:
    certificates:
      - cert_file: "/etc/linkc/fullchain.pem"
        key_file:  "/etc/linkc/privkey.pem"

storage:
  path: "/var/lib/linkc-node"

logging:
  level: "info"
  format: "json"
```

要点：

- `node_type` 固定 `vless`，其他值会被拒绝；
- 端口由面板下发，本地只决定监听地址 `node.listen`；
- `tls.certificates` 仅在面板返回 `tls_type=tls` 时必需，支持多组证书按 SNI 选择；
- `api_key_file` 内容只取首尾去空白的整行，适合 systemd 或密钥挂载；
- 所有标量支持 `${ENV_NAME}` 展开，未设置的变量会直接报错。

### 3.1 systemd 环境变量（可选）

unit 默认不加载环境文件。若坚持用 `${PANEL_API_KEY}`，给 unit 增加：

```ini
EnvironmentFile=-/etc/linkc-node/env
```

```bash
echo 'PANEL_API_KEY=xxxx' | sudo tee /etc/linkc-node/env >/dev/null
sudo chmod 0600 /etc/linkc-node/env && sudo chown linkc:linkc /etc/linkc-node/env
```

## 4. 首次启动与验证

```bash
# 1) 只校验本地配置、证书、目录，不访问面板
sudo -u linkc linkc-node check -c /etc/linkc-node/config.yaml

# 2) 拉取面板配置并打印脱敏后的 Xray JSON（会访问面板，失败回退 last_good）
sudo -u linkc linkc-node dump-config -c /etc/linkc-node/config.yaml

# 3) 前台试跑，确认无误后交给 systemd
sudo -u linkc linkc-node run -c /etc/linkc-node/config.yaml
```

systemd 方式：

```bash
sudo systemctl enable --now linkc-node
sudo systemctl status linkc-node
sudo journalctl -u linkc-node -f
```

启动成功的判断标准：

```bash
# readyz 返回 200 且 ready=true
curl --unix-socket /run/linkc-node/admin.sock http://localhost/readyz
# status 中 users 数量与面板一致、last_error 为空
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
```

同时在面板后台确认节点在线、用户数与流量正常增长。客户端侧用面板下发的
VLESS 链接连接，验证 TCP / WS+TLS / REALITY 等实际使用的传输方式。

## 5. 常用操作

### 5.1 审计：先观察，再阻断

```yaml
node:
  audit:
    enabled: true
    action: "report"        # 先 report，观察命中日志与面板审计记录
    rule_mode: "auto"
    whitelist_file: "/etc/linkc-node/whitelist.txt"
```

确认误报可接受后改成 `action: "block"` 并重启（配置变更需 restart，SIGHUP 只重载证书）。
白名单每行一条，支持 `#` 注释。

### 5.2 限速、设备数与 TCP 限制

```yaml
node:
  limits:
    mode: "enforce"                 # enforce | report | off
    device_limit_ipv6_prefix: 128   # 按 /64 聚合设备时改为 64
    burst_bytes: 65536
```

节点全局限速与用户限速来自面板；本地 `limits.mode` 控制设备数/TCP 限制是否真正执行。

### 5.3 流量上报与在线 IP

```yaml
node:
  traffic:
    include_online_zero: false   # true 时额外上报 0/0 在线用户，站点 online_user 更准
    batch_users: 500
  alive_ip:
    mode: "changed"              # changed 只发变更；full 每次全量
    full_every: 20               # changed 模式下每 20 次补一次全量
```

流量是 at-least-once：采集后先写 bbolt journal，推送成功才删除。面板已写入但响应丢失时
重发会重复计数，站点侧无幂等键，属已知行为。

### 5.4 证书轮换（不中断连接）

```bash
# 替换证书文件后
sudo systemctl reload linkc-node    # SIGHUP -> 重新读取 node.tls.certificates
```

- 新证书生效方式为“新建 inbound → 移除旧 inbound”，失败时保留旧配置；
- 只重载证书，不重载 YAML；改配置请 `systemctl restart`；
- 验证完成前保留旧证书文件。

### 5.5 多实例

使用模板 unit，每个实例一份配置与状态目录：

```bash
sudo cp deploy/linkc-node@.service /etc/systemd/system/
sudo cp /etc/linkc-node/config.yaml /etc/linkc-node/node2.yaml
# 修改 node2.yaml 的 storage.path / admin.socket，避免与实例 1 冲突
sudo systemctl enable --now linkc-node@node2
```

### 5.6 面板故障时的行为

- 启动时优先使用 `last_good`，面板不可达仍能启动并继续代理；
- 运行中拉取失败保持当前用户与配置，连续 3 次失败进入 `degraded`，恢复后自动追平；
- 流量与审计事件继续落盘，POST 失败保留待重发；
- `/readyz` 只反映核心与本地状态，`/status` 的 `degraded` 才表示面板异常。

## 6. 管理面与监控

```bash
linkc-node status -c /etc/linkc-node/config.yaml
linkc-node metrics -c /etc/linkc-node/config.yaml
linkc-node ready -c /etc/linkc-node/config.yaml
# 或直接通过 curl 查询 Unix socket
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
```

- `/status`：用户数、在线 IP、待上报流量、`last_success` / `last_error`，排障先看这里；
- `/metrics`：Prometheus 指标，抓取方式与告警规则见 [monitoring.md](./monitoring.md)；
- `admin.pprof: true` 可临时开启性能分析，生产用完即关。

## 7. 排障速查

| 现象 | 先查 |
|---|---|
| 节点显示离线 | `journalctl -u linkc-node` 有无 401/403/503；系统时间；`checkNodeIp` 出口 IP |
| 面板负载显示 2350% | 是否误设 `status.mode=doc`，保持 `load_fraction` |
| 用户连不上 | UUID/端口/REALITY 参数是否与面板一致；`/status`；证书与审计阻断日志 |
| 审计误封 | 改回 `action: report`；检查白名单、`rule_mode`、sniffing |
| 流量重复计费 | 看 `linkc_traffic_pending_bytes`；确认是否响应丢失导致重发 |
| 启动即退出 | `check` 的报错；`state.db` schema 不兼容会直接报错，不会静默改库 |

更多问答见 [faq.md](./faq.md)。

## 8. 升级与回滚

```bash
sudo systemctl stop linkc-node
sudo cp -a /var/lib/linkc-node/state.db /var/lib/linkc-node/state.db.bak
sudo install -m 0755 linkc-node /usr/local/bin/linkc-node
sudo systemctl start linkc-node
```

回滚时同时恢复旧二进制与 `state.db.bak`。完整步骤见 [upgrade.md](./upgrade.md)。

## 9. 相关文档

- [quickstart.md](./quickstart.md)：一键启动脚本（`scripts/quickstart.sh`）
- [configuration.md](./configuration.md)：全部配置字段
- [operations.md](./operations.md)：部署、Docker、备份、面板故障
- [monitoring.md](./monitoring.md)：指标、抓取、告警
- [security-audit.md](./security-audit.md)：安全模型与加固
- [upgrade.md](./upgrade.md)：升级、回滚、证书轮换
- [faq.md](./faq.md)：常见问题
- [p0-live-validation.md](./p0-live-validation.md)：真实站点联调清单
