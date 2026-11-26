# Contributing

感谢你参与 `linkc-node`。提交 issue 或 pull request 前，请先确认问题属于本项目，
并阅读相关文档。

## Before opening an issue

- 搜索现有 issue 和 release notes，避免重复报告。
- 使用对应的问题模板，提供版本、系统、部署方式和可复现步骤。
- 不要公开 API Key、UUID、私钥、面板地址中的敏感参数或生产配置。
- 安全漏洞请按 [SECURITY.md](SECURITY.md) 私下报告。

## Development setup

```bash
git clone https://github.com/Beiyu114514/linkc-bc-soga-v1.git
cd linkc-bc-soga-v1

go mod download
make build
make test
make test-launcher
make test-quickstart
```

需要 Go 1.27 或更高版本。修改 shell 脚本后请运行 `shellcheck`，修改 Python
启动器后请运行 `make test-launcher`。

## Pull requests

- 保持变更范围聚焦，一个 PR 解决一个明确问题。
- 新增行为必须包含测试；修复缺陷时优先添加能复现问题的回归测试。
- 运行 `gofmt`，并提交前执行：

```bash
make test
make race
make vet
make test-launcher
make test-quickstart
git diff --check
```

- 更新用户可见行为时同步修改 README、docs 和 `CHANGELOG.md`。
- 在提交信息中使用清晰的 Conventional Commit 前缀，例如
  `fix(deploy): ...`、`feat(cli): ...` 或 `docs: ...`。
- 建议为每个提交添加 `Signed-off-by`，表示同意
  [Developer Certificate of Origin 1.1](https://developercertificate.org/)。

## Release process

维护者发布新版本时：

1. 更新 `CHANGELOG.md` 和 `docs/release-notes-X.Y.Z.md`。
2. 确认 CI 通过。
3. 创建 `vX.Y.Z` tag 并推送。
4. `.github/workflows/release.yml` 构建 amd64/arm64 二进制、deb/rpm、
   `LICENSE`、第三方声明和 `SHA256SUMS`，并创建 GitHub Release。
5. 确认 Docker workflow 发布对应版本、`major.minor` 和 `latest` 镜像。

发布候选版本使用带预发布后缀的 tag，例如 `v0.13.0-rc.1`。

## License of contributions

提交贡献即表示你有权提供该内容，并同意其按照本仓库当前的 [LICENSE](LICENSE)
分发。第三方代码必须保留其原始许可证和版权声明。
