# FAQ

### quickstart 一键启动后仍显示离线

- 脚本最后一次 `ready`/`status` 的结论比面板显示更早：先看 `sudo linkc-node status -c /etc/linkc-node/config.yaml`；
- `--wait 0` 会跳过就绪检查，此时服务已启动但脚本不保证可用；
- 未就绪时脚本会打印 journal 尾部，常见原因是面板不可达、API Key 错误、下发端口被占用。

### 节点显示离线

- 检查 `node_heartbeat`、本机系统时间和面板 pull 间隔；
- 看 `journalctl -u linkc-node` 是否有 401/403/503；
- `checkNodeIp=true` 时确认出口 IP 与后台一致。

### 面板负载显示 2350%

- 本程序默认 `status.mode=load_fraction`，发送 `load="0.235000"`；
- 如果仍显示异常，检查是否误设 `status.mode=doc` 或站点版本不一致。

### 用户连不上

- 确认 UUID、端口、REALITY 参数与面板一致；
- 查看 `/run/linkc-node/admin.sock` 的 `/status`；
- 看日志是否有 REALITY/证书/审计阻断。

### 审计误封

- 生产先使用 `audit.action=report`；
- 检查白名单和 `rule_mode`；
- 确认 sniffing 是否拿到域名；纯 IP/ECH/HTTP3 可能漏判。

### 流量重复计费

- traffic 是 at-least-once；面板无幂等键；
- 面板已写入但响应丢失时，重试会重复累加；
- 查看 `linkc_traffic_pending_bytes` 与站点节点带宽。
