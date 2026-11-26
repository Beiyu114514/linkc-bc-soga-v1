# Docker 一键部署与管理（CLI）

`scripts/docker-launch.sh` 是纯命令行部署与管理工具（不启动任何 Web 服务或端口）：通过交互式向导或子命令参数生成完整的 `config.yaml`、`api-key`（root 下 0640，非 root 下 0600）、可选 TLS 证书和 `docker-run.sh`，并直接通过 Docker 编排容器生命周期及查询本地 Unix socket 管理接口。

适合第一次部署、自动化脚本批量部署以及不想手写 YAML 的场景。

## 前置条件

- Linux 主机（`--network host` 是必需的）；
- Docker 已安装并可用（`docker info` 成功）；
- Python 3.8+（仅依赖标准库，无需 `pip install`）；
- 能访问面板的 `base_url`，并已取得 `node_id` 与 API Key。

## 快速开始

### 1. 交互式向导（推荐首次部署）

```bash
sudo ./scripts/docker-launch.sh wizard
```

按提示输入配置/数据目录、面板地址、`node_id`、API Key（隐藏回显）、可选 TLS 证书路径以及审计/限速开关，预览生成的 `config.yaml` 与 `docker run` 命令后即可直接部署或仅生成文件（`stage`）。

### 2. 非交互一键部署

生产默认目录（`/etc/linkc-node`、`/var/lib/linkc-node`）：

```bash
printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/docker-launch.sh deploy \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-stdin
```

或从环境变量读取 API Key（避免出现在 `ps` 命令行中）：

```bash
sudo env PANEL_API_KEY="$PANEL_API_KEY" ./scripts/docker-launch.sh deploy \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-env PANEL_API_KEY
```

非 root（写入 `~/.linkc-node/etc`、`~/.linkc-node/data`，需用户在 `docker` 组）：

```bash
./scripts/docker-launch.sh deploy \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-env PANEL_API_KEY
```

## 常用子命令

| 命令 | 说明 |
|---|---|
| `wizard` (`init`) | 交互式配置向导，确认后执行 `deploy` 或 `stage` |
| `deploy` | 校验参数、拉取/构建镜像、执行 `check`、备份并部署容器（失败自动回滚） |
| `preview` | 仅在终端输出校验告警、`config.yaml` 与 `docker run` 命令，不写文件 |
| `stage` | 只生成 `config.yaml`、`api-key`、证书与 `docker-run.sh`，不操作容器 |
| `check` | 使用临时隔离容器运行 `linkc-node check` 校验配置 |
| `status` (`state`) | 查看 Docker、托管容器、配置文件路径与告警状态 |
| `logs` | 查看或跟踪容器日志（`-n 200`、`-f`） |
| `start` / `stop` / `restart` / `remove` | 启停、重启或删除由启动器托管的容器 |
| `node-status` / `node-metrics` / `node-ready` | 直接通过本地 Unix socket（`admin.sock`）查询运行中的节点状态 |

常用参数示例：

```bash
# 查看全部参数
./scripts/docker-launch.sh --help
./scripts/docker-launch.sh deploy --help

# 预览生成结果（支持 --json 输出）
sudo ./scripts/docker-launch.sh preview --panel-url https://panel.example.com --node-id 12 --json

# 仅生成配置文件并加载 TLS 证书
sudo ./scripts/docker-launch.sh stage \
  --panel-url https://panel.example.com \
  --node-id 12 \
  --api-key-env PANEL_API_KEY \
  --cert-file /path/to/fullchain.pem \
  --key-file /path/to/privkey.pem

# 使用 JSON 参数文件（与 launcher.json 同结构）进行自动化部署
sudo ./scripts/docker-launch.sh deploy --config-json ./node.json --api-key-env PANEL_API_KEY

# 本地源码构建并部署
sudo ./scripts/docker-launch.sh deploy --build-local --image linkc-node:local

# 容器与节点运维
sudo ./scripts/docker-launch.sh status
sudo ./scripts/docker-launch.sh logs -n 100 -f
sudo ./scripts/docker-launch.sh restart
sudo ./scripts/docker-launch.sh node-ready
sudo ./scripts/docker-launch.sh node-status
sudo ./scripts/docker-launch.sh node-metrics
```

也可直接用 `linkc-node` 二进制自身命令查询宿主机暴露出的管理 socket（Docker 生成的 `config.yaml` 中 `admin.socket` 为容器内路径 `/run/linkc-node/admin.sock`，因此宿主机上请通过 `-s` 指定映射路径；非 root 默认位于 `~/.linkc-node/data/run/admin.sock`）：

```bash
linkc-node status -s /var/lib/linkc-node/run/admin.sock
linkc-node metrics -s /var/lib/linkc-node/run/admin.sock
linkc-node ready -s /var/lib/linkc-node/run/admin.sock
```

## 部署顺序与自动回滚

`deploy` 的执行顺序为：

1. 拉取（`--pull-latest`）或本地构建（`--build-local`）镜像；
2. 在临时目录生成配置并用隔离容器（`--network none`）执行 `linkc-node check`；
3. 校验通过后备份既有配置文件到 `backups/<时间戳>/` 并写入新配置；
4. 停止旧容器并重命名暂存，随后启动新容器；
5. 在启动后 10 秒内检查容器是否保持运行且未重启；若失败则抓取日志、删除新容器、恢复旧配置与旧容器名并按原状态重启旧容器。

## 生成的文件

默认位置（root 下；非 root 下位于 `~/.linkc-node/`）：

| 文件 | 说明 |
|---|---|
| `/etc/linkc-node/config.yaml` | 完整配置（0640，容器 UID 10001 可读） |
| `/etc/linkc-node/api-key` | 面板 API Key（0640，不写入 `config.yaml`/`launcher.json`/`docker-run.sh`） |
| `/etc/linkc-node/certs/fullchain.pem` | 可选 TLS 证书 |
| `/etc/linkc-node/certs/privkey.pem` | 可选 TLS 私钥 |
| `/etc/linkc-node/whitelist.txt` | 可选审计白名单 |
| `/etc/linkc-node/docker-run.sh` | 可复现的 `docker run` 脚本（0700） |
| `/etc/linkc-node/launcher.json` | 供后续 `deploy`/`status` 复用的非敏感参数（0600） |
| `/etc/linkc-node/backups/<时间>/` | 覆盖前备份（0700），失败回滚时自动恢复 |
| `/var/lib/linkc-node/state.db` | bbolt 状态、`last_good` 与流量 journal |
| `/var/lib/linkc-node/run/admin.sock` | 宿主机上的管理 Unix socket |

> 配置目录和数据目录必须不存在、为空，或只包含本启动器/节点管理的文件；指向已有其他内容的系统目录（如 `/var/lib`、`/usr/local`、家目录）会被拒绝。所有路径均拒绝符号链接与 `:` 字符。

## 自检

```bash
python3 -m unittest discover -s scripts -p 'test_*.py' -v
bash -n scripts/docker-launch.sh
```
