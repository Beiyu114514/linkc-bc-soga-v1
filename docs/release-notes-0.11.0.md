# linkc-node 0.11.0

## 新增

- **Docker 图形化启动器**：`scripts/docker-launch.sh` 启动一个带随机 token 的本机参数页，
  填写面板地址 / API Key / 节点 ID（可选证书、白名单）后生成 `config.yaml`、`api-key`、
  `certs/`、`docker-run.sh` 并启动容器。只用 Python 标准库。
  - 非回环监听强制 `--username`；支持自动生成密码、`--ask-password`、scrypt 哈希文件。
  - 登录失败按来源 IP 限流；Host/Origin 校验阻止 DNS rebinding 和跨站 POST。
  - API Key 只写入 0600 的 `api-key` 文件，不进入预览、`docker run` 命令或 `launcher.json`。
  - 配置/数据目录只接管空目录或只含启动器/节点文件的目录。
- `docs/usage.md`：从安装、配置、启动验证到常用操作和排障的完整使用文档。
- `docs/docker-launcher.md`：启动器安全模型、参数、生成文件、管理面和常见问题。
- CI 新增 `launcher` job；本地可用 `make test-launcher` 跑 47 个 stdlib 单测。

## 修复

- 启动器失败回滚现在会先从 `backups/<时间>/` 恢复旧配置，再启动旧容器，
  避免旧容器读取新的坏配置后一起退出。
- 同名非托管容器改为在写配置前拒绝，避免“部署被拒绝但配置已被覆盖”。
- `docker run` 失败时先清理可能残留的 Created 容器，避免旧容器改名回滚失败。

## 升级

```bash
# 二进制 / deb / rpm：见 GitHub Release 资产
# Docker：
docker pull ghcr.io/beiyu114514/linkc-bc-soga-v1:0.11.0
```

启动器用法见 `docs/docker-launcher.md`；节点配置与运维见 `docs/usage.md`。
