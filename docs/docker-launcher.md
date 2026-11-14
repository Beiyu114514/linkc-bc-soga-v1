# Docker 一键启动（图形化）

`scripts/docker-launch.sh` 会在本机启动一个图形化参数页：填写面板地址、API Key、节点 ID 等参数后，页面会生成完整的 `config.yaml`、`api-key`、可选证书和 `docker-run.sh`，然后调用 Docker 启动 `linkc-node`。默认监听 `0.0.0.0`，网络内可访问；非回环监听必须设置用户名/密码，否则启动器拒绝启动。

适合第一次部署、临时节点，以及不想手写 YAML 的场景。生产环境仍然建议把生成的配置纳入配置管理。

## 前置条件

- Linux 主机（`--network host` 是必需的）；
- Docker 已安装并可用（`docker info` 成功）；
- Python 3.8+（只用标准库，不需要 pip 安装任何东西）；
- 能访问面板的 `base_url`；
- 面板侧已创建 VLESS 节点，拿到 `node_id` 与 API Key。

## 快速开始

生产默认目录（`/etc/linkc-node`、`/var/lib/linkc-node`）：

```bash
# 默认监听 0.0.0.0；自动生成并打印页面密码
sudo ./scripts/docker-launch.sh --username admin
# 终端会打印密码，并写入 0600 JSON 凭据文件
```

> 默认监听 `0.0.0.0`。**必须加 `--username` 启用登录**（否则拒绝启动），或用 `--bind 127.0.0.1` 仅本机访问。

非 root（使用 `~/.linkc-node/etc`、`~/.linkc-node/data`，需要当前用户在 `docker` 组）：

```bash
./scripts/docker-launch.sh --username admin
```

脚本会打印类似下面的地址，并尝试自动打开浏览器：

```text
http://127.0.0.1:随机端口/?token=...
http://<服务器IP>:随机端口/?token=...
```

如果服务器没有图形环境，复制这个地址到本地浏览器，或先做 SSH 端口转发：

```bash
ssh -L 8080:127.0.0.1:8080 user@server
# 然后手动指定端口（默认 0.0.0.0，需要登录）：
./scripts/docker-launch.sh --username admin --port 8080 --no-browser
# 或者仅本机：
./scripts/docker-launch.sh --bind 127.0.0.1 --port 8080 --no-browser
```

常用参数：

```bash
./scripts/docker-launch.sh --help
./scripts/docker-launch.sh --username admin --no-browser
./scripts/docker-launch.sh --username admin --ask-password
./scripts/docker-launch.sh --username admin --port 8080
./scripts/docker-launch.sh --username admin --no-credentials-file
./scripts/docker-launch.sh --username admin --credentials-file /etc/linkc-launcher/credentials.json
./scripts/docker-launch.sh --hash-password --hash-password-output /root/.linkc-node/password.hash
./scripts/docker-launch.sh --username admin --password-hash-file /root/.linkc-node/password.hash
./scripts/docker-launch.sh --bind 127.0.0.1
```

`--bind` 默认是 `0.0.0.0`；非回环监听必须用 `--username`，仅本机用 `--bind 127.0.0.1`。

## 用户名密码（0.0.0.0 必需）

监听 `0.0.0.0` 时必须设置用户名密码，否则启动器拒绝启动；仅本机 token 模式用 `--bind 127.0.0.1`。启用方式：

```bash
sudo ./scripts/docker-launch.sh --username admin
```

默认会自动生成随机密码，打印到终端，并写入 0600 JSON 凭据文件（默认
`~/.linkc-node/launcher-credentials.json`）。想自己输入密码用 `--ask-password`；也可以用 scrypt
哈希文件（见下）；非交互环境可以用环境变量：

> 启动器不会把密码写入 `config.yaml` / `launcher.json` / `docker-run.sh`；凭据文件包含带
> token 的 URL，按密码同等保护。

```bash
sudo env LINKC_LAUNCHER_USER=admin LINKC_LAUNCHER_PASSWORD='...' \
  ./scripts/docker-launch.sh --no-browser
```

也可以直接传 `--password`，但它会出现在 `ps` 里，不建议。

### 凭据文件

自动生成的密码会写入 0600 JSON 文件，默认位置：

- root / sudo：`/root/.linkc-node/launcher-credentials.json`
- 非 root：`~/.linkc-node/launcher-credentials.json`

读取：

```bash
sudo cat /root/.linkc-node/launcher-credentials.json
# 或
cat ~/.linkc-node/launcher-credentials.json
```

内容示例（JSON）：

```json
{
  "generated_at": "2026-11-14T11:57:52Z",
  "listen": "0.0.0.0",
  "port": 8080,
  "url": "http://127.0.0.1:8080/?token=REPLACE_ME",
  "lan_url": "http://192.168.1.10:8080/?token=REPLACE_ME",
  "username": "admin",
  "password": "REPLACE_ME"
}
```

读取密码：

```bash
sudo jq -r '.password' /root/.linkc-node/launcher-credentials.json
# 没有 jq 就直接 cat
sudo cat /root/.linkc-node/launcher-credentials.json
```

- 每次自动生成密码时覆盖为新的随机密码；
- `--credentials-file /path` 改路径，`LINKC_LAUNCHER_CREDENTIALS_FILE` 也可指定；`--no-credentials-file` 不写文件；
- 只有“自动生成”的密码会写文件；`--ask-password` / `--password` / 环境变量提供的密码不会写。
  显式传 `--credentials-file` 但密码不是自动生成时，启动器会直接报错，避免静默忽略；
- 文件包含带 token 的 URL，请按密码同等保护；
- 自定义路径请指向专用文件，不要指向 `/dev`、`/proc`、`/sys` 或系统配置文件；启动器会覆盖目标路径。

> 旧的无后缀路径 `launcher-credentials` 不再写入；它是旧的 key=value 格式，不要直接改名为 `.json`。需要保留就手动转成 JSON，或重新启动生成新的 JSON 文件。

### 从文件读取密码哈希

不想让启动器保存明文密码时，可以用 scrypt 哈希文件：

```bash
# 1) 生成哈希文件（提示输入两次密码；文件 0600）
sudo ./scripts/docker-launch.sh --hash-password \
  --hash-password-output /root/.linkc-node/password.hash

# 2) 启动时读取哈希（仍需 --username）
sudo ./scripts/docker-launch.sh --username admin \
  --password-hash-file /root/.linkc-node/password.hash
```

哈希文件支持两种格式：

```text
scrypt$16384$8$1$<base64 salt>$<base64 hash>
```

```json
{
  "algorithm": "scrypt",
  "n": 16384,
  "r": 8,
  "p": 1,
  "salt": "base64...",
  "hash": "base64..."
}
```

要点：

- `--hash-password` 只生成哈希并退出，不启动页面；`--hash-password-output` 写 0600 文件。
- 不写 `--hash-password-output` 时只打印哈希，自己保存到 0600 文件。
- `--password-hash-file` 需要同时提供 `--username`；不能和 `--password` / `--ask-password` / `LINKC_LAUNCHER_PASSWORD` 同时使用。
- 哈希文件在启动时读取；改哈希后重启启动器。
- 哈希模式下启动器不保存明文密码，也不会写 `launcher-credentials.json`。
- 哈希文件本身也是敏感文件（可离线爆破），保持 0600。
- 环境变量 `LINKC_LAUNCHER_PASSWORD_HASH_FILE` 可指定哈希文件。
- 生成哈希时也可以用 `--password` 或 `LINKC_LAUNCHER_PASSWORD` 提供密码（自动化用，注意 `ps`）。

启用后浏览器会弹出 HTTP Basic Auth 登录框；页面 URL 里的 token 仍然需要，两层都通过才进入。用户名不能包含冒号，密码可以包含冒号。启动器只在内存中使用凭据；自动生成的密码写入 0600 JSON 凭据文件，重启后从文件读取新密码或重新生成。

> Basic Auth 在纯 HTTP 下只做 Base64 编码，不加密。页面默认监听 `0.0.0.0`；请用防火墙限制来源，或通过 SSH 转发/反向代理并在前面加 HTTPS。

## 页面怎么用

1. **面板连接**：填 `base_url`、API Key、`node_id`。`base_url` 必须和面板授权路径一致；末尾斜杠会自动去掉。
2. **节点与安全**：
   - 面板下发 `tls` 时上传 PEM 证书和私钥；REALITY 不需要证书。
   - 审计动作建议先用 `report`，观察误报后再改 `block`。
   - 白名单每行一条，支持 `#` 注释。
3. **Docker**：
   - 默认镜像 `ghcr.io/beiyu114514/linkc-bc-soga-v1:latest`。
   - 勾选“使用本地源码构建镜像”会执行 `docker build`，镜像名默认改成 `linkc-node:local`。
   - “启动前执行 linkc-node check”会先用同一份配置跑一次 `check`，失败就不启动、也不覆盖现有配置。
4. 右侧预览会实时显示 `config.yaml`、`docker run` 命令和 `docker-run.sh`。确认后点“生成配置并启动”。

部署顺序是：拉取/构建镜像 → 在临时目录生成配置并 `check` → 写入配置（先备份旧文件）→ 停旧容器、改名暂存 → 启动新容器。
新容器在 10 秒内退出或发生重启会被判定失败，启动器会删除新容器并把旧容器改回原名、按原状态恢复；
只有新容器稳定运行后才会删除暂存的旧容器。

页面下方可以查看容器状态、启动/停止/重启、读取日志、删除容器。删除前会先 `docker stop`，给进程时间落盘。

> 配置目录和数据目录必须不存在、为空，或只包含本启动器/节点自己的文件（`config.yaml`、`api-key`、
> `certs/`、`whitelist.txt`、`docker-run.sh`、`launcher.json`、`backups/`、`env`；数据目录为
> `state.db*`、`run/`）。指向 `/var/lib`、`/usr/local`、家目录这类已有其他内容的目录会被拒绝，
> 因为启动器会把这些目录改成 `0700` 并（root 下）chown 给容器用户。

## 生成的文件

默认位置：

| 文件 | 说明 |
|---|---|
| `/etc/linkc-node/config.yaml` | 完整配置，0600 |
| `/etc/linkc-node/api-key` | 面板 API Key，0600 |
| `/etc/linkc-node/certs/fullchain.pem` | 可选证书，0600 |
| `/etc/linkc-node/certs/privkey.pem` | 可选私钥，0600 |
| `/etc/linkc-node/whitelist.txt` | 可选审计白名单，0600 |
| `/etc/linkc-node/docker-run.sh` | 可重复执行的 `docker run` 命令，0755 |
| `/etc/linkc-node/launcher.json` | 页面回填用的非敏感参数，0600 |
| `/etc/linkc-node/backups/<时间>/` | 覆盖前的旧文件备份 |

页面登录凭据文件默认在 `~/.linkc-node/launcher-credentials.json`（JSON，0600），不在配置目录里，
不会被挂载进容器。

数据目录：

| 文件 | 说明 |
|---|---|
| `/var/lib/linkc-node/state.db` | bbolt 状态、last_good、流量 journal |
| `/var/lib/linkc-node/run/admin.sock` | 宿主机上的管理 socket |

容器内路径固定为：

- 配置：`/etc/linkc-node`（只读挂载）
- 数据：`/var/lib/linkc-node`
- 管理 socket：`/run/linkc-node/admin.sock`

## 管理面

页面会把管理 socket 挂到宿主机：

```bash
curl --unix-socket /var/lib/linkc-node/run/admin.sock http://localhost/readyz
curl --unix-socket /var/lib/linkc-node/run/admin.sock http://localhost/status
curl --unix-socket /var/lib/linkc-node/run/admin.sock http://localhost/metrics
```

非 root 模式下，把路径换成 `~/.linkc-node/data/run/admin.sock`。

## 安全说明

- 页面默认监听 `0.0.0.0`，每次启动生成随机 token；页面 URL 和 API 请求都需要 token。
- 监听 `0.0.0.0` 时必须用 `--username` 启用登录（否则拒绝启动）；仅本机可用 `--bind 127.0.0.1`。
- HTTP Basic Auth 用户名/密码（`--username`）；非回环监听必需，启用后与 token 叠加，两层都通过才进入。
- 自动生成的密码写入 0600 JSON 凭据文件（默认 `~/.linkc-node/launcher-credentials.json`）；文件包含带 token 的 URL，请按密码保护。
- 可选 scrypt 哈希文件（`--password-hash-file`）；哈希文件 0600，启动器不保存明文密码。
- 连续 10 次登录失败后，同一来源 IP 会被限流 60 秒（返回 429 + `Retry-After`）；哈希校验最多 4 个并发，避免并发错误密码把启动器打到 OOM。
- 服务端校验 `Host` 和 `Origin`，阻止 DNS rebinding 和跨站 POST。页面只接受 `localhost` 或 IP 形式的 Host；用域名直接访问会被拒绝，需要域名时请在前置反向代理处理 Host/HTTPS。
- 配置目录/数据目录只接管空目录或只含本启动器/节点文件的目录；其他目录直接拒绝，不做递归 chmod/chown。
- 未挂载 geodata 时页面会提示：容器内没有 `geosite.dat`/`geoip.dat`，面板下发的 geosite/geoip 审计规则会被跳过（等于放行）。
- API Key 只写入本机 `0600` 的 `api-key` 文件；不会出现在页面预览、`docker run` 命令、`docker-run.sh` 或 `launcher.json` 中。
- 证书/私钥只写入本机 `0600` 文件；不会回填到页面。
- 页面没有第三方依赖、没有 CDN、没有外部字体。
- 不要把带 `?token=...` 的地址发给别人；用完可以点“关闭页面服务”。关闭页面不会停止容器。

## Docker 命令

生成的命令等价于：

```bash
docker run -d \
  --name linkc-node \
  --network host \
  --restart unless-stopped \
  --stop-timeout 30 \
  --init \
  --log-driver json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  --label com.linkc.managed-by=linkc-docker-launcher \
  --security-opt no-new-privileges:true \
  -v /etc/linkc-node:/etc/linkc-node:ro \
  -v /var/lib/linkc-node:/var/lib/linkc-node \
  -v /var/lib/linkc-node/run:/run/linkc-node \
  ghcr.io/beiyu114514/linkc-bc-soga-v1:latest
```

必须使用 `--network host`：端口映射会丢失真实源 IP，影响 `alive_ip` 和设备数限制。

## 常见问题

**页面打不开 / 提示需要 token**

复制终端里完整地址（包含 `?token=`）。如果通过 SSH 转发，确认本地端口和 `--port` 一致。

**页面一直弹登录框 / 忘记密码**

先看凭据文件（JSON）：

```bash
sudo jq -r '.password' /root/.linkc-node/launcher-credentials.json
# 没有 jq 就直接 cat
sudo cat /root/.linkc-node/launcher-credentials.json
```

如果用的是 `--password-hash-file`，启动器没有明文密码；忘记密码就重新生成哈希文件。
如果用了 `--no-credentials-file` 或文件被删掉，重新启动启动器会生成新的随机密码并打印；
想自己输入密码用 `--ask-password`：

```bash
sudo ./scripts/docker-launch.sh --username admin
# 或
sudo ./scripts/docker-launch.sh --username admin --ask-password
```

如果不需要登录，改用 `--bind 127.0.0.1` 重启（仅本机 token 模式）。

**怎么用哈希文件，不保存明文密码**

见上面的“从文件读取密码哈希”。简单说：

```bash
sudo ./scripts/docker-launch.sh --hash-password --hash-password-output /root/.linkc-node/password.hash
sudo ./scripts/docker-launch.sh --username admin --password-hash-file /root/.linkc-node/password.hash
```

**用域名打开被拒绝**

页面只接受 `localhost` 或 IP 形式的 Host，这是防 DNS rebinding 的设计。用 `http://服务器IP:端口/...` 访问；如果必须用域名，请在前置反向代理里处理 Host 和 HTTPS。

**页面暴露到网络了**

默认 `--bind 0.0.0.0`，启动器会强制要求 `--username`。如果不需要远程访问，改用
`--bind 127.0.0.1`；需要远程访问时，加 `--username` 启用登录，并用防火墙/安全组只放行可信来源。
页面里的“对外监听”标签会提示当前状态。

**Docker 不可用**

先确认 `docker info` 能成功。非 root 用户需要加入 `docker` 组：

```bash
sudo usermod -aG docker "$USER"
# 重新登录后生效
```

**同名容器已存在，但不是启动器创建的**

启动器只会删除带 `com.linkc.managed-by=linkc-docker-launcher` 标签的容器。手动创建的 `linkc-node` 容器需要先自行删除，或在页面里换一个容器名称。

**容器启动后很快退出**

看页面里的日志。最常见原因是面板不可达且没有 `last_good`；也可能是证书/私钥不匹配、端口被占用、配置校验失败。
启动器会在启动后 10 秒内观察容器：如果容器退出或发生重启，会判定失败、删除新容器并把旧容器恢复（改名回原名称，
原来在运行就重新启动，原来已停止就保持停止），错误信息里会带上容器状态和重启次数。先在页面点“保存并校验”，再点“日志”。

**低端口（<1024）绑定失败**

Docker 默认带 `CAP_NET_BIND_SERVICE`，但 rootless Docker 或某些安全策略可能限制。优先让面板分配高位端口，或按目标机要求调整 Docker 权限。

**SELinux 拒绝挂载**

在页面勾选“SELinux：给配置/数据目录加 `:z` 重新标记”。geodata 目录不会自动加 `:z`，避免影响其他服务；如被拒绝，请手动调整标签或把 geodata 复制到配置目录。

**想回到手动 docker run**

`/etc/linkc-node/docker-run.sh` 就是完整的启动命令，不包含任何密钥。改完配置后可以直接执行它。

**升级镜像**

页面里勾选“启动前拉取最新镜像”，再点“生成配置并启动”。数据目录会保留；启动器会先 `docker stop`
再重建容器。回滚时把镜像改回旧 tag，重新部署即可。新容器起不来时启动器会自动回滚到旧容器。

## 自检

启动器只依赖 Python 标准库，参数校验、配置生成、认证和容器健康判断都有单测（不需要 Docker 或 root）：

```bash
python3 -m unittest discover -s scripts -p 'test_*.py' -v
bash -n scripts/docker-launch.sh
```

CI 的 `launcher` job 会跑这两项，并检查 `docker-launcher.html` 内联脚本能否解析。

## 清理

```bash
# 停止并删除容器（保留配置和数据）
docker stop linkc-node
docker rm linkc-node

# 确认不再需要后，再删除目录
sudo rm -rf /etc/linkc-node /var/lib/linkc-node
```

删除数据目录会丢失 `state.db`（包括 last_good 和待上报流量 journal），请先备份。
