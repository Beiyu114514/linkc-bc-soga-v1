# 升级与回滚

## 升级前

```bash
systemctl stop linkc-node
cp -a /var/lib/linkc-node/state.db /var/lib/linkc-node/state.db.bak
cp -a /etc/linkc-node/config.yaml /etc/linkc-node/config.yaml.bak
```

## 替换二进制

```bash
install -m 0755 linkc-node /usr/local/bin/linkc-node
systemctl start linkc-node
systemctl status linkc-node
journalctl -u linkc-node -n 100
```

数据库 schema 在启动时检查；不兼容会报错退出，不会静默修改数据库。

## 配置兼容性

0.10.0 起移除了已无作用的 `alive_ip.clear_offline` 字段，YAML 解析是严格的。从 0.8.x 升级时请先删除该行，否则 `linkc-node check` 会报 `field clear_offline not found`。

## 回滚

```bash
systemctl stop linkc-node
install -m 0755 linkc-node.previous /usr/local/bin/linkc-node
cp -a /var/lib/linkc-node/state.db.bak /var/lib/linkc-node/state.db
systemctl start linkc-node
```

## 证书轮换

1. 替换 `/etc/linkc/*.pem`；
2. `systemctl reload linkc-node`（SIGHUP）触发 `ReloadCerts`；
3. 用客户端验证新证书；
4. 保留旧文件到验证完成。

SIGHUP 只重载证书，不重载 YAML 配置；配置变更需要 restart。
