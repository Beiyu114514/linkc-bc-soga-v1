# 一键启动（quickstart）

`scripts/quickstart.sh` 把「写配置 → 校验 → 安装 → 启动 → 等就绪」串成一条命令，
适合第一次部署、重装节点以及批量初始化。它不是新的部署体系：底层仍然是
`linkc-node check`、`deploy/linkc-node.service` 与 `scripts/install.sh`，
Docker 模式则直接转调 [`scripts/docker-launch.sh`](./docker-launcher.md)。

## 前置条件

- Linux 节点机（amd64 / arm64），root 或 sudo 权限；
- 面板侧已创建 VLESS 节点，拿到 `node_id` 与 API Key；
- 面板下发的端口已放行；
- 使用 TLS 时证书文件路径已就绪（REALITY 无需本地证书）；
- systemd 模式需要 Go 工具链（本地没有 `./linkc-node` 时会执行 `make build`）；
  Docker 模式需要已安装 Docker；
- 脚本依赖**仓库内的文件**（`scripts/install.sh`、`Makefile`、`deploy/linkc-node.service`），
  因此在任意目录执行都可以（脚本会自己定位仓库根），但仓库必须存在；
  只用 deb/rpm 安装的机器不需要它，直接改 `/etc/linkc-node/config.yaml` 后
  `systemctl restart linkc-node` 即可。

## 快速开始

```bash
# 1. 非交互一键启动（API Key 走标准输入，不会出现在 ps 中）
printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-stdin

# 2. 交互式向导（逐步询问，回车用默认值；非 root 时先提权再提问）
sudo ./scripts/quickstart.sh wizard

# 3. 只预览：打印将生成的配置与执行步骤，不写任何文件
./scripts/quickstart.sh --dry-run \
  --panel-url https://panel.example.com --node-id 12 --api-key-env PANEL_API_KEY
```

成功时输出类似：

```
==> 构建二进制: make build
==> 安装二进制、用户与 systemd unit
==> 写入配置: /etc/linkc-node/config.yaml
==> 校验配置: /usr/local/bin/linkc-node check -c /etc/linkc-node/config.yaml
config ok
==> 启用并重启服务: systemctl enable linkc-node && systemctl restart linkc-node
==> 等待节点就绪（最多 30s）

✅ linkc-node 已启动并就绪
```

未就绪时脚本会附上最近 6 行 journal 日志，便于直接定位（面板不可达、API Key 错误、
端口被占用、证书不可读是最常见的四类）。

## 参数

| 参数 | 说明 |
|---|---|
| `--panel-url URL` | 面板地址，必须 `https://`（测试环境加 `--allow-http`） |
| `--node-id ID` | 面板节点 ID |
| `--api-key-stdin` | 从标准输入读一行作为 API Key |
| `--api-key-env NAME` | 从环境变量读取（推荐；直接运行脚本让脚本自行 sudo，变量值不会进入 `ps`） |
| `--api-key-file PATH` | 从文件读取 |
| `--api-key VALUE` | 直接写在命令行，会暴露在 `ps`，不推荐 |
| `--profile NAME` | 面板协议，默认 `sspanel-uim-soga` |
| `--listen IP` | 监听地址，默认 `0.0.0.0`（端口由面板下发） |
| `--cert-file` / `--key-file` | TLS 证书对，必须成对出现 |
| `--limits-mode` | `enforce`（默认）/ `report` / `off` |
| `--audit-action` | `report`（默认，先观察）/ `block` |
| `--no-audit` | 关闭审计规则 |
| `--geodata-dir DIR` | `geoip.dat`/`geosite.dat` 目录，默认 `/usr/share/xray` |
| `--log-level` / `--log-format` | 默认 `info` / `json` |
| `--mode` | `auto`（默认）/ `systemd` / `docker` / `foreground` |
| `--image` | Docker 模式镜像，默认 `ghcr.io/beiyu114514/linkc-bc-soga-v1:latest` |
| `--stage-only` | 只写配置并本地校验，不启动服务（二进制与 unit 仍会安装） |
| `--dry-run` | 只打印配置与命令，不写文件 |
| `--force` | 覆盖已存在的 `config.yaml` |
| `--build` | 强制重新 `make build` |
| `--wait SECONDS` | 启动后等待就绪的秒数，默认 30（`0` 表示只启动、不做就绪检查） |

完整列表见 `./scripts/quickstart.sh --help`。

## 三种运行模式

| 模式 | 行为 | 适用场景 |
|---|---|---|
| `systemd`（默认*） | 构建二进制 → `scripts/install.sh` 安装用户/unit → 写配置 → `check` → `systemctl enable` + `restart` → 等 `readyz` | 生产部署、裸机或 VPS |
| `docker` | 转调 `scripts/docker-launch.sh deploy`，生成配置与容器；启动后用 `node-ready` 等就绪 | 不想在宿主机装二进制 |
| `foreground` | 用仓库内二进制前台运行；配置存在则用它，否则在临时目录生成一份并在退出后删除 | 调试、临时验证 |

\* `--mode auto` 时：有 systemd 用 `systemd`，否则有 Docker 用 `docker`，都没有则 `foreground`。

## 生成的配置文件

写入 `/etc/linkc-node/config.yaml`（0640，`root:linkc`），API Key 写在
`/etc/linkc-node/api-key`（0600，`linkc:linkc`），凭 `api_key_file` 引用。配置目录为
`0750 root:linkc`，服务用户只能读取配置，不能替换文件。默认生成的内容：

```yaml
panel:
  base_url: "https://panel.example.com"
  profile: "sspanel-uim-soga"
  api_key_file: "/etc/linkc-node/api-key"
  node_id: "12"
  node_type: "vless"
  timeout: 15s
  allow_http: false
  follow_redirects: false
  tls:
    verify: true

node:
  listen: "0.0.0.0"
  tls:
    certificates: []          # 传 --cert-file/--key-file 时自动写入证书对

  limits:
    mode: "enforce"

  audit:
    enabled: true
    action: "report"          # 先观察，确认无误再改 block
    rule_mode: "auto"
    whitelist_file: "/etc/linkc-node/whitelist.txt"

  geodata:
    dir: "/usr/share/xray"

storage:
  path: "/var/lib/linkc-node"
  max_journal_bytes: 67108864

logging:
  level: "info"
  format: "json"

admin:
  socket: "/run/linkc-node/admin.sock"
  metrics: true
  pprof: false
```

生成的是**最小可用配置**，其余字段使用程序默认值（见
[configuration.md](./configuration.md)）。已有配置默认保留，加 `--force` 才会覆盖，
覆盖前会把旧文件备份为 `config.yaml.bak.<时间戳>.<随机后缀>`（最多保留最近 5 份）；
需要更细的字段请直接编辑该文件后 `systemctl restart linkc-node`。

## 常见用法

```bash
# 已有节点，只想换成 systemd 托管（保留现有配置）
./scripts/quickstart.sh --panel-url https://panel.example.com --node-id 12 \
  --api-key-env PANEL_API_KEY

# 带 TLS 证书、直接开阻断
sudo ./scripts/quickstart.sh --panel-url https://panel.example.com --node-id 12 \
  --api-key-stdin --cert-file /etc/linkc/fullchain.pem --key-file /etc/linkc/privkey.pem \
  --audit-action block < /etc/linkc-node/api-key

# Docker 方式部署
./scripts/quickstart.sh --mode docker --panel-url https://panel.example.com \
  --node-id 12 --api-key-env PANEL_API_KEY

# 只生成配置，稍后手工启动
./scripts/quickstart.sh --stage-only --panel-url https://panel.example.com \
  --node-id 12 --api-key-env PANEL_API_KEY
```

非 root 运行时脚本会自动 `sudo` 重新执行（`systemd` 模式，或当前用户无 docker 权限的 `docker` 模式），
通过变量名保留 `--api-key-env` 所指向的变量与 `LINKC_*` 覆盖项，API Key 不进入 sudo 命令行；
`--dry-run` 与 `foreground` 模式不提权。使用 `--api-key-env` 时不要在外层再包一层
`sudo`，否则普通 sudo 会先清空该环境变量；直接运行脚本即可。

## 脚本做了什么

1. 校验参数（面板地址必须 https、`node_id` 不能含空格等）；
2. 构建二进制（已有 `./linkc-node` 且未加 `--build` 时跳过）；
3. 复用 `scripts/install.sh`：创建 `linkc` 用户，安装 `/usr/local/bin/linkc-node`
   与 `/etc/systemd/system/linkc-node.service`；配置目录为 `root:linkc` 0750，
   状态与运行目录为 `linkc:linkc` 0700；
4. 原子写入 `config.yaml`（`root:linkc` 0640）、`api-key` 与空 `whitelist.txt`
   （均为 `linkc:linkc` 0600）；
5. `linkc-node check` 做本地校验（不访问面板）；
6. `systemctl daemon-reload && systemctl enable linkc-node && systemctl restart linkc-node`；
7. 轮询 `linkc-node ready` 直到 `readyz` 返回 200 或超时，然后打印一次 `status`。

脚本是**幂等**的：重复执行会重装二进制与 unit、刷新 API Key，并显式重启服务，
因此升级时新二进制与配置会立即生效；已存在的 `config.yaml` 默认保留（改配置请编辑文件或加
`--force`，覆盖前自动备份），用户与目录不会重复创建。

## 与其它部署方式的关系

| 方式 | 入口 | 说明 |
|---|---|---|
| 一键启动（本文） | `scripts/quickstart.sh` | 生成最小配置 + 安装 systemd 服务 + 启动 |
| 手动安装 | `scripts/install.sh` | 只安装二进制/unit，配置需自己准备 |
| Docker 一键部署 | `scripts/docker-launch.sh` | 交互向导或全参数化，生成完整配置并管理容器 |
| 安装包 | `linkc-node_*.deb` / `.rpm` | `postinstall` 创建用户与目录，配置需自己准备 |

三者共用同一份配置语义，互相切换时保留 `/etc/linkc-node/config.yaml` 与
`/var/lib/linkc-node/state.db` 即可（Docker 模式映射的是同样两个目录）。

## 环境变量覆盖（测试/预发）

正式环境不要使用，unit 文件里的路径是写死的（`foreground` 模式例外，它始终用 `mktemp -d` 的临时目录）：

| 变量 | 默认值 |
|---|---|
| `LINKC_BIN_DIR` | `/usr/local/bin` |
| `LINKC_CONF_DIR` | `/etc/linkc-node` |
| `LINKC_STATE_DIR` | `/var/lib/linkc-node` |
| `LINKC_RUN_DIR` | `/run/linkc-node` |
| `LINKC_UNIT_DIR` | `/etc/systemd/system` |
| `LINKC_SKIP_SYSTEMCTL` | `0`，设 `1` 时不执行 `systemctl` |
| `LINKC_IMAGE` | Docker 模式默认镜像 |

`foreground` 模式的临时目录不受上述变量影响，始终使用 `mktemp -d` 生成的位置。
覆盖值必须是绝对路径；脚本会解析符号链接，拒绝系统级目录（如 `/`、`/etc`、`/usr/local/bin`）
以及彼此重叠的安装目录。自定义目录的名称或其父目录名称必须使用 `linkc-node` 专用名称
（例如 `/etc/linkc-node-test`、`/var/lib/linkc-node-test` 或 `/mnt/linkc-node/data`）。
目录中受管的配置文件也不允许是符号链接。

## 排障

| 现象 | 处理 |
|---|---|
| `配置校验失败` | 按 `check` 的报错改 `config.yaml`，常见是 `panel.base_url` 非 https、证书路径不存在 |
| `⚠️ 未就绪` | 看脚本附带的 journal 尾部：面板不可达 / API Key 401 / 端口占用 / 证书不可读 |
| `找不到 go 工具链` | 装 Go，或先 `make linux-amd64` 再把二进制放到仓库根目录 `./linkc-node` |
| Docker 模式报 python 缺失 | 需要 Python 3.8+（`docker-launch.sh` 依赖） |
| `--force` 覆盖了想保留的配置 | 覆盖前会自动备份为 `config.yaml.bak.<时间戳>.<随机后缀>`（最近 5 份），从中恢复即可 |

启动后的日常操作见 [usage.md](./usage.md)，指标与告警见 [monitoring.md](./monitoring.md)。
