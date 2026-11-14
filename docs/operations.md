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

```bash
docker build -t linkc-node .
docker run --rm --network host \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  linkc-node
```

必须使用 `--network host`：端口映射会丢失真实源 IP，影响在线 IP 与设备数限制。

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
- 多证书按 SNI 选择已支持；真实 CA 多域名场景待上线验证。
- 审计依赖 sniffing；纯 IP、ECH/ESNI、HTTP/3 可能看不到域名。
- `dump-config` 仅用于排查，不要直接作为生产配置启动。
- 24h 长稳与目标机 systemd/SELinux 兼容性待验收（见 `docs/security-audit.md`）。
