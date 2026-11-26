# 运维说明（M1）

## 构建与检查

```bash
go build -trimpath -ldflags "-s -w" -o linkc-node ./cmd/linkc-node
./linkc-node check -c /etc/linkc-node/config.yaml
./linkc-node dump-config -c /etc/linkc-node/config.yaml   # 脱敏输出
```

`check` 只验证本地配置/证书/目录，不访问面板。`dump-config` 会访问面板（失败时回退 last_good）。

## 构建与安装

```bash
make linux-amd64          # 生成 dist/linkc-node-linux-amd64
make build                # 当前架构
make test race vet
sudo ./scripts/install.sh
```

`scripts/install.sh` 会创建 `linkc` 用户、目录、安装 systemd unit；配置文件不存在时安装示例配置。

## 一键启动

推荐首次部署使用一键启动脚本（生成最小配置、安装 unit、`check`、启动并等就绪）：

```bash
printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
  --panel-url https://panel.example.com --node-id 12 --api-key-stdin
```

其它子模式：`--mode docker` 转调 `scripts/docker-launch.sh deploy`，`--stage-only` 只写配置，
`--dry-run` 只预览，`wizard` 为交互向导。脚本是幂等的（已存在的 `config.yaml` 默认保留，`--force` 覆盖）。
详见 [quickstart.md](./quickstart.md)。

升级时同样执行这条命令即可：不加 `--force` 会保留现有 `config.yaml`，服务会被重启。

`install.sh` 可用 `LINKC_BIN_DIR` / `LINKC_CONF_DIR` / `LINKC_STATE_DIR` / `LINKC_RUN_DIR` /
`LINKC_UNIT_DIR` 覆盖安装路径，`LINKC_SKIP_EXAMPLE_CONFIG=1` 跳过示例配置，
`LINKC_SKIP_SYSTEMCTL=1` 跳过 `systemctl`（仅供测试/打包使用）。

## systemd

手动安装：

```bash
useradd --system --home /var/lib/linkc-node --shell /usr/sbin/nologin linkc
install -d -m 0700 -o linkc -g linkc /etc/linkc-node /var/lib/linkc-node
install -m 0600 -o linkc -g linkc config.yaml /etc/linkc-node/config.yaml
install -m 0644 deploy/linkc-node.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now linkc-node
journalctl -u linkc-node -f
```

低端口依赖 `CAP_NET_BIND_SERVICE`。证书/配置目录保持只读；`ProtectHome=true` 时不要把证书放在 `/home`。

## Docker

命令行一键部署（推荐首次使用）：

```bash
# 交互式向导
sudo ./scripts/docker-launch.sh wizard

# 或非交互式一键部署
sudo env PANEL_API_KEY="$PANEL_API_KEY" ./scripts/docker-launch.sh deploy \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-env PANEL_API_KEY

# 查看状态、日志与节点管理接口
sudo ./scripts/docker-launch.sh status
sudo ./scripts/docker-launch.sh node-status
sudo ./scripts/docker-launch.sh logs -f
```

`docker-launch.sh` 为纯命令行部署与管理工具（无 Web 页面），会自动生成配置、API Key 文件、可选证书和 `docker-run.sh`。详见
[docker-launcher.md](./docker-launcher.md)。若只想一条命令搞定，可用
`sudo ./scripts/quickstart.sh --mode docker ...`，它会带上这些参数转调 `deploy`。

手动方式：

```bash
docker build -t linkc-node .
docker run --rm --network host \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  -v /var/lib/linkc-node/run:/run/linkc-node \
  linkc-node
```

必须使用 `--network host`：端口映射会丢失真实源 IP，影响在线 IP 与设备数限制。

镜像内 `linkc-node` 已设置 `CAP_NET_BIND_SERVICE` 文件能力，容器仍以 UID 10001 运行，因此面板下发 440/443 等低端口也能绑定。不要额外加 `--security-opt no-new-privileges:true`，它会禁用该文件能力；只有确认端口 >=1024 时才建议开启。

预构建镜像（由 `.github/workflows/docker.yml` 推送，多架构 amd64/arm64）：

```bash
docker pull ghcr.io/beiyu114514/linkc-bc-soga-v1:latest
docker run --rm --network host \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  -v /var/lib/linkc-node/run:/run/linkc-node \
  ghcr.io/beiyu114514/linkc-bc-soga-v1:latest
```

镜像内附带示例配置 `/etc/linkc-node/config.yaml.example`；运行时以 UID 10001 执行，
状态目录需对该 UID 可写（`chown -R 10001:10001 /var/lib/linkc-node`）。

## 面板故障

- 启动时优先使用 `last_good`；面板不可达仍可启动并继续代理。
- 运行中 GET 失败保持当前用户/配置，连续 3 次进入 `degraded`；恢复后自动追平。
- 流量/journal 继续落盘；POST 失败保留待上报，恢复后重发。
- `/readyz` 只反映核心与本地状态；`/status` 的 `degraded` 表示面板拉取异常。

## 流量与 at-least-once

- 采集时 `Set(0)` 读取并清零，随后写入 bbolt journal。
- 推送成功后才删除 journal 记录。若“面板已写入但响应丢失”，重发会重复累加；目标站点没有幂等键。
- journal 达到 `max_journal_bytes` 后停止采集并告警，代理服务不受影响。

## 备份/升级

```bash
systemctl stop linkc-node
cp -a /var/lib/linkc-node/state.db /var/lib/linkc-node/state.db.bak
systemctl start linkc-node
```

升级二进制后重启；schema 不兼容会在启动时报错，不会静默改库。回滚时同时恢复 `state.db.bak`。

## SIGHUP 证书轮换

```bash
systemctl reload linkc-node     # SIGHUP -> Agent.ReloadCerts
```

- 重新读取 `node.tls.certificates` 文件；
- 证书变化时新建 inbound -> 移除旧 tag；
- 失败保留旧 inbound；
- 仅重载证书，不重载 YAML。

## 多证书 / SNI

`node.tls.certificates` 支持多组证书：

```yaml
node:
  tls:
    certificates:
      - cert_file: /etc/linkc/fullchain.pem
        key_file:  /etc/linkc/privkey.pem
      - cert_file: /etc/linkc/other.pem
        key_file:  /etc/linkc/other.key
```

Xray 按 SNI 选择证书。

## 管理面

```bash
curl --unix-socket /run/linkc-node/admin.sock http://localhost/status
curl --unix-socket /run/linkc-node/admin.sock http://localhost/metrics
```

指标包括 `linkc_ready`、`linkc_degraded`、`linkc_users`、`linkc_online_ips`、
`linkc_traffic_pending_bytes`、`linkc_audit_pending`、限额与审计计数器；
完整清单、抓取方案与告警规则见 [monitoring.md](./monitoring.md) 与 `deploy/alerts.yml`。

## M3 已具备

- `audit_rules` / `white_list` 拉取、编译、report/block、按 `(user, rule)` 去重入队、`/audit_log` 推送。
- `alive_ip` 变更检测与推送；`full` / `changed` 模式。
- `traffic.include_online_zero` 可补齐站点在线数。
- 节点/用户限速、设备数（IPv6 /64 聚合）、TCP 连接数限制；`enforce` / `report` / `off`。
- 审计指标：`linkc_audit_rules`、`linkc_audit_hits_total`、`linkc_audit_reported_total`、`linkc_audit_blocked_total`、`linkc_audit_pending`。

## 仍限制

- ECH 实际握手未验证（站点不下发 ECH key，缺合法测试 key set）。
- geosite 属性（`@cn` 等）与 `geoip` reverse 语义未实现。
- 多证书按 SNI 选择已支持；真实 CA 多域名场景待上线验证。
- 审计依赖 sniffing；纯 IP、ECH/ESNI、HTTP/3 可能看不到域名。
- `dump-config` 仅用于排查：服务运行中无法读取 state DB，会退化为仅访问面板。
- 24h 长稳与目标机 systemd/SELinux 兼容性待验收（见 `docs/security-audit.md`）。
