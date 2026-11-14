# 安全审计

> 范围：`deploy/linkc-node.service`、`scripts/install.sh`、`packaging/nfpm.yaml`、
> 凭据处理、输入信任边界、依赖管理。
> 结论：**修复 M5 review 中的 P1 后可上线**；下列为已完成的缓解与遗留建议。

## 1. 进程与系统层

| 控制项 | 现状 | 说明 |
|---|---|---|
| 专用低权用户 | ✅ `useradd --system --shell nologin linkc` | install.sh 创建系统用户，服务以 `User=linkc` 运行 |
| `NoNewPrivileges=true` | ✅ | 禁止 setuid 提权链 |
| `ProtectSystem=strict` + `ReadWritePaths` | ✅ | 只读根文件系统，仅 `/var/lib/linkc-node`、`/run/linkc-node` 可写 |
| `ProtectHome/PrivateTmp/PrivateDevices` | ✅ | 隔离 home、/tmp、设备节点 |
| `ProtectKernelTunables/Modules/ControlGroups`、`LockPersonality` | ✅ | 阻止内核参数/模块/控制组篡改 |
| `RestrictSUIDSGID/Realtime`、`SystemCallArchitectures=native` | ✅ | 最小化攻击面 |
| `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX` | ✅ | 仅放行协议实际使用的地址族 |
| 低端口（443 等） | ✅ `AmbientCapabilities=CAP_NET_BIND_SERVICE` + `CapabilityBoundingSet` 收紧 | 仅保留绑定低端口能力，不给全部 root |
| `UMask=0077`（systemd）+ CLI 启动 `umask 077` | ✅ | 状态库/证书/key 落盘 0600 |
| 重启策略 | ✅ `Restart=on-failure`，`StartLimitIntervalSec=0` | 崩溃后自动恢复；配合 journal 保证流量不丢（at-least-once） |

**Docker**：镜像以 UID 10001 运行，推荐 `network_mode: host` + 只读根文件系统
（见 `Dockerfile` 与 `docs/operations.md`）。

## 2. 凭据与密钥

- **API key**：仅来自环境变量或 0600 配置文件；`dump-config` 对 UUID/私钥/证书脱敏；
  日志不记录 key（面板客户端只记录路径与状态码）。
- **REALITY 私钥**：由面板下发，仅存在于内存与生成的 Xray 配置中，不落盘；
  日志不输出私钥。
- **TLS 私钥**：以 inline PEM 形式读入内存（规避 Xray 文件热加载的上游 data race），
  文件权限 0600。
- **面板 mTLS**：客户端证书/key 通过 `panel.tls.client_cert_file/client_key_file` 提供，
  启动时校验匹配。

## 3. 输入信任边界

面板响应被视为不可信输入，已有控制：

| 风险 | 缓解 |
|---|---|
| 超大响应打爆内存 | `panel.max_response_bytes`（默认 32MB）+ 超限即报错 |
| 恶意/异常 JSON | 每端点结构化解码 + 字段校验（`node.FromPanel`、`NormalizeUsers`） |
| 用户列表投毒（无效 UUID/重复/负限额） | `ValidateUsers`：UUID 规范化后碰撞检测、限额非负、ID 去重 |
| 大规模误删用户 | `node.users.mass_remove_guard` 安全阀 + `allow_empty` 仅告警不拒绝 |
| 审计规则注入 | 规则按 `auto` 模式编译（regex/keyword），编译失败跳过并计数 |
| 证书/私钥伪造 | `tls.X509KeyPair` 启动校验；ECH key base64 校验 |
| 路径穿越 | 白名单/证书均按文件路径读取，无用户可控路径拼接 |

## 4. 依赖与供应链

- Go module 锁定（`go.mod`/`go.sum`）；Xray-core 固定 commit 伪版本
  `v1.260327.1-0.20261109052709-...`（GitHub tag 不是合法 module 版本，见风险 R22）。
- 无 CGO，静态单二进制，减少运行时依赖。
- 升级流程见 `docs/upgrade.md`（含回滚）。

## 5. 已知遗留与建议

| # | 项 | 状态 | 建议 |
|---|---|---|---|
| S1 | systemd 沙箱在真实发行版的兼容性（旧版 systemd 缺少某些指令） | 未实测 | 灰度前在目标发行版 `systemd-analyze verify` + 启动冒烟 |
| S2 | SELinux/AppArmor | 未配置 | 若目标机强制 SELinux，需为 `/var/lib/linkc-node`、`/run/linkc-node` 配置 file context；容器场景可加 AppArmor profile |
| S3 | 管理面 socket 权限 | ✅ 0600 + 目录 0700 | 保持；不要暴露到 TCP |
| S4 | 面板 401/403 处理 | ✅ 报错并记 degraded | 无需改动 |
| S5 | 合规/授权 | ⚠️ 运营侧责任 | 上线前确认运营合规；本实现未复制 soga 代码，契约依站点源码复刻 |

## 6. 验证方式

```bash
# 单元/契约/混沌测试（含面板故障、限额、审计、证书轮换）
make test
make race

# 24h 长稳（随机故障注入）
SOAK_DURATION=24h go test ./internal/agent -run TestSoak -count=1 -timeout 25h

# systemd 单元静态检查（在有 systemd 的机器上执行）
systemd-analyze verify deploy/linkc-node.service
```
