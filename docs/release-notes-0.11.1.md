# linkc-node 0.11.1

## 修复

- **Docker 低端口**：镜像内 `/usr/local/bin/linkc-node` 设置 `CAP_NET_BIND_SERVICE`
  文件能力，容器仍以 UID 10001 运行，面板下发 440/443 等低端口也能绑定。
- **Docker 启动器**：`no-new-privileges` 默认关闭（它会禁用上述文件能力）；
  页面会提示低端口场景，CI 增加 `getcap` 校验。
- **deb/rpm 安装包**：postinstall 创建 `linkc` 用户并修正
  `/etc/linkc-node`、`/var/lib/linkc-node` 权限，安装后 systemd 可直接启动；
  补充 `passwd`/`libc-bin`（deb）和 `shadow-utils`/`glibc-common`（rpm）依赖。

## 升级

- 从 0.8.x 升级：删除 `alive_ip.clear_offline`；0.10.0 起严格解析会拒绝该字段。
- Docker 用户请重新拉取 `0.11.1` 或 `latest`；0.11.0 镜像没有低端口文件能力。
