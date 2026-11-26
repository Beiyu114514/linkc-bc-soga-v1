# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的结构，
版本号使用语义化版本。

## [Unreleased]

暂无。

## [0.12.0] - 2026-11-26

### Added

- 新增 `scripts/quickstart.sh`，支持 `systemd`、Docker、前台、向导、dry-run 和
  stage-only 部署。
- 新增安装路径约束、配置备份、就绪检查和安装脚本回归测试。
- 新增 README、贡献指南、安全政策、行为准则、问题模板和发布说明。
- 发布资产现在包含许可证和第三方依赖声明。

### Changed

- Docker 部署管理改为纯命令行启动器，移除内置 Web 页面和浏览器依赖。
- 安装包和 quickstart 统一使用只读配置模型：配置目录 `root:linkc 0750`，
  配置文件 `root:linkc 0640`。
- 系统服务升级时显式执行 `daemon-reload`、`enable` 和 `restart`。
- Docker 管理命令的输出、日志尾随、健康状态和错误 JSON 结构更稳定。

### Fixed

- 修复 quickstart 在路径覆盖、符号链接、sudo 环境变量、向导输入和 dry-run
  中的安全问题与行为不一致。
- 修复 admin socket 在缺失或无效配置下的回退行为，避免误连默认 socket。
- 修复 Xray 运行时、流量统计、限额、审计、geodata 和面板同步中的多处竞态或错误。
- 修复 deb/rpm 安装脚本对用户、目录和升级重启的处理。

### Security

- API Key 不再进入 sudo 命令行。
- 安装目录禁止指向重叠目录、系统级目录或非专用路径。
- 受管文件拒绝符号链接并原子替换。

## [0.11.1] - 2026-11-14

### Fixed

- Docker 镜像恢复低端口绑定 capability。
- deb/rpm 安装时创建服务用户并修正配置与状态目录权限。

## [0.11.0] - 2026-11-14

### Added

- Docker 部署启动器和管理命令。
- 完整使用、部署和监控文档。

[Unreleased]: https://github.com/Beiyu114514/linkc-bc-soga-v1/compare/v0.12.0...HEAD
[0.12.0]: https://github.com/Beiyu114514/linkc-bc-soga-v1/compare/v0.11.1...v0.12.0
[0.11.1]: https://github.com/Beiyu114514/linkc-bc-soga-v1/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/Beiyu114514/linkc-bc-soga-v1/releases/tag/v0.11.0
