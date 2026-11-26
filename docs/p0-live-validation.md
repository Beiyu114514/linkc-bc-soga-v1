# P0 真实站点联调清单

> 目标：用真实测试节点确认 mock 无法回答的语义。  
> 前置：站点后台可创建节点，能拿到 `NODE-ID` / `API-KEY`，节点出口 IP 固定。

## 1. 建测试节点

在 SSPanel-UIM 后台创建 **VLESS** 节点（建议单独测试节点，不要复用生产）：

- `sort`: `15`（sspanel 的 VLESS 类型）
- 端口：测试端口（如 24443）
- server：按传输填写，例如
  - TLS + Vision：`example.com;24443;0;tcp;tls;sni=example.com|flow=xtls-rprx-vision`
  - REALITY：`example.com;24443;0;tcp;reality;sni=www.example.com|flow=xtls-rprx-vision|private_key=...|short_ids=...|dest=www.example.com:443`
  - WS + TLS：`example.com;24443;0;ws;tls;sni=example.com|path=/ws`
- `node_ip`: 节点出口 IP（若站点开启 `checkNodeIp`）
- 记录 `NODE-ID` 与 `muKey`

## 2. 本地配置

```yaml
panel:
  base_url: "https://panel.example.com/"
  api_key: "${PANEL_API_KEY}"
  node_id: "<NODE-ID>"
  node_type: "vless"
  allow_http: false
  tls:
    verify: true
node:
  listen: "0.0.0.0"
  tls:
    certificates:
      - cert_file: "/etc/linkc/fullchain.pem"
        key_file:  "/etc/linkc/privkey.pem"
  freedom:
    allow_private_targets: false
  audit:
    enabled: true
    action: "report"        # 先 report，不要直接 block
    rule_mode: "auto"
  users:
    allow_empty: true
  status:
    mode: "load_fraction"
```

```bash
go build -o linkc-node ./cmd/linkc-node
PANEL_API_KEY=... ./linkc-node check -c /etc/linkc-node/config.yaml
PANEL_API_KEY=... ./linkc-node run -c /etc/linkc-node/config.yaml
```

## 3. 逐项验证

### 3.1 基础

- [ ] 日志出现 `bootstrap from panel` 或 `bootstrap from last_good`
- [ ] `/node` 返回的 `node_type`、端口、传输与后台一致
- [ ] 客户端能连，HTTP 目标可访问
- [ ] 面板侧用户流量增加
- [ ] 面板侧节点负载显示为百分数（例如 23.5%，不是 2350%）
- [ ] `alive_ip` 出现在站点在线 IP 页

### 3.2 `status.load`

- 在节点上执行 `top`，比较面板“负载”与 CPU 使用率
- 记录：`load` 第一 token 是 **CPU 使用率 0..1**，还是 **load average**
- 若显示 2350%，说明 `status.mode` 配错或面板期望 load average

### 3.3 空 users

- 把测试用户流量改成超配额（或让节点 `node_bandwidth_limit < node_bandwidth`）
- [ ] `GET /users` 返回 `[]`
- [ ] 节点不报错，应用空用户
- [ ] `keep_connect=true` 时用户仍在，`speed_limit` 变为 1

### 3.4 审计裸 regex

在后台创建一条测试规则（不要影响生产）：

```text
.*google\.com
```

- 访问 `http://www.google.com/`（或任何匹配域名）
- 记录：
  - 节点日志是否出现 `audit hit ... domain=www.google.com`
  - 面板 `detect_log` 是否出现记录
- 再建一条 `keyword` 形式规则（如 `google`），确认两种语义差异
- 结论决定生产 `audit.rule_mode`：`auto` / `regex` / `keyword`

### 3.5 在线数

- 让 2 个用户同时在线
- 观察面板 `ss_node_online_log.online_user`
- 对比：
  - 默认（只发有流量用户）
  - `traffic.include_online_zero=true`
- 确认站点是否要求包含 0 流量在线用户

### 3.6 限额

- 设置用户 `speed_limit=1`、`device_limit=1`
- [ ] 4 Mbps 视频/下载被限制在 ~1 Mbps
- [ ] 第二台设备被拒绝（或按站点期望行为）
- 确认 `speed_limit`/`device_limit` 是否需要节点强制执行

### 3.7 面板故障

- 停掉面板（或断网）
- [ ] 节点继续接受新连接
- [ ] 日志进入 degraded
- [ ] 恢复后自动追平
- [ ] 重启节点（面板仍不可达）能从 `last_good` 启动

## 4. 记录模板

| 项目 | 结果 | 备注 |
|---|---|---|
| 线上源码与归档一致 | 是/否 | 行级 diff |
| `status.load` 语义 | CPU 使用率 / load average | |
| 审计裸 regex | regex / keyword | |
| 在线数含 0 流量 | 是/否 | |
| 限额强制执行 | 是/否 | |
| 证书来源 | 本地文件 / ACME / 其他 | |
| `checkNodeIp` | 开/关 | |
| 反代 | 有/无 | `REMOTE_ADDR` |
| `xtls` 配置 | 有/无 | |

将结果回填到 `M2-REPORT.md` / `PLAN.md` §17，再决定 M3 的 `rule_mode` 与限额策略。
