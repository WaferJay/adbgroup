# 开发与发版

[项目首页](../README.zh-CN.md) · [English](development.md)

## 运行测试

在仓库根目录用 Go 1.26+ 运行完整测试。

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

shell 集成测试要求 PATH 中有 bash、zsh 和 `script`，并允许本地 socket 监听。Linux 上的 `script` 由 util-linux 提供，macOS 自带；缺少工具时测试会立即报错。

端到端测试用内存连接运行真实 mTLS、HTTP Upgrade 和 WebSocket，覆盖 ADB 数据、客户端 forward/reverse、身份撤销、租约绑定和断线清理；不要求 OS TCP 监听权限。原生 adb 测试需要安装 adb 且允许 TCP 监听，否则跳过。真实 USB 验收需共享设备、同组两设备及另组设备，并发 shell、文件传输和转发后拔插单台，确认其他设备连接不中断。

无外部依赖时可使用 Go 1.22+ 单独验证核心和 PKI：

```sh
GOTOOLCHAIN=local go test -modfile=test/core.mod -race ./internal/gateway ./internal/pki
```

macOS 新系统配旧 Go 1.22 若遇 missing LC_UUID，可加 `-ldflags=-linkmode=external`。生产构建及完整验收使用 Go 1.26+。

## Docker 镜像

[Docker 镜像工作流](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/docker.yml) 构建 `linux/amd64` 和
`linux/arm64` 镜像。面向 `main` 的 PR 仅验证构建；推送到 `main`、推送 `v*`
标签或手动运行工作流时，使用内置 `GITHUB_TOKEN` 发布到
`ghcr.io/waferjay/adbgroup`，无需额外配置镜像仓库密钥。

构建镜像前，工作流使用 Go 1.26 的最新补丁版本运行 `go vet ./...` 和
`go test -race -count=1 -timeout=5m ./...`，并安装 ADB 执行原生客户端兼容性测试，
安装 bash、zsh 和 util-linux 提供的 `script` 执行 shell 集成测试。检查失败时不会构建或发布镜像。

`latest` 和 `main` 镜像标签跟随 `main` 分支；`v1.0.0` 等 Git 标签生成同名镜像
标签，每次发布还会生成 `sha-<短提交号>` 标签。

首次发布后，如需匿名拉取镜像，请将 GHCR 包的可见性设为 public。[Docker 部署](../docker/README.md)

## 二进制发版

推送 `v0.1.0` 或 `v0.1.0-beta.1` 等语义化版本标签会触发
[CI 与发版工作流](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/docker.yml)。测试和 Docker 镜像发布成功后，
[发版任务](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/release.yml) 使用 [GoReleaser](https://github.com/WaferJay/adbgroup/blob/main/.goreleaser.yaml)
创建 GitHub Release，包含：

- Linux、macOS 的 `amd64`、`arm64` 二进制压缩包，命名为
  `adbgroup_<版本>_<系统>_<架构>.tar.gz`；
- SHA-256 校验文件 `checksums.txt`；
- 根据上一标签之后的提交生成的发布说明。

每个压缩包附带中英文 README、`docs/` 指南、服务端和客户端配置示例、systemd 服务及 Linux
USB 规则。Linux 二进制可运行服务端和客户端，macOS 二进制用于客户端。
带预发布后缀的标签自动标记为预发布，发布使用内置 `GITHUB_TOKEN`。

提交并推送发版相关修改后，发布版本：

```sh
git tag -a v0.1.0-beta.1 -m "Release v0.1.0-beta.1"
git push origin v0.1.0-beta.1
```

本地安装 GoReleaser v2.20.0 后，可验证打包而不发布：

```sh
goreleaser check
goreleaser release --snapshot --clean
```

快照压缩包和校验文件位于 `dist/`。
