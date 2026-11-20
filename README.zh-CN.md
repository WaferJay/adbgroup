# ADB 分组网关

[English](README.md) | **简体中文**

`adg` 提供本地 ADB 隧道，让你访问通过 USB 接在共享 Linux 主机上的 Android 设备。网关通过双向 TLS（mTLS）认证用户，按组授予访问权限。同一台设备可以属于多个组。需要独占设备时，可以申请跨组生效的写租约。

## 安装 adg

在 macOS 或 Linux 终端运行以下命令，即可安装或升级到最新发布的版本（含预发布版）。

```sh
curl -fsSL https://raw.githubusercontent.com/WaferJay/adbgroup/main/install.sh | sh
```

脚本校验 SHA-256 后将 `adg` 安装到 `~/.local/bin`。请将该目录加入 `PATH`。如需运行原生 `adb` 命令，也请加入 Android SDK Platform Tools 的目录。

[手动安装与指定版本](docs/client.zh-CN.md#安装选项) · [为 Codex CLI 或 Claude Code 安装 adg 技能](docs/client.zh-CN.md#安装-adg-技能)

## 连接设备

通过私密渠道向网关管理员领取 `client.adgpack`，其中包含你的客户端证书和私钥。导入后列出分组和设备，将示例中的 `team-a` 和 `USB_SERIAL_A` 换成查询到的组名和在线设备序列号。

```sh
adg profile import client.adgpack
adg groups
adg devices team-a
adg team-a -s USB_SERIAL_A shell getprop ro.product.model
```

## 启动 Linux 网关

在连接 USB 设备的主机上安装 `adg` 和 Android SDK Platform Tools，并给运行服务的账号授予 USB 设备访问权限。将 `config.example.yaml` 复制为 `config.yaml`，把 `backend.binary` 设为本机的 `adb` 路径，填入设备序列号和用户身份，并在 `pki.server_names` 中填写客户端连接时使用的域名或 IP。[配置示例](config.example.yaml)

```sh
adg check -config config.yaml
adg serve -config config.yaml
```

使用示例配置首次启动后，Alice 的凭据包位于 `run/pki/clients/alice/client.adgpack`。通过私密渠道交给 Alice 即可导入。

## 文档

| 指南 | 内容 |
| --- | --- |
| [客户端指南](docs/client.zh-CN.md) | 安装选项、智能体技能、凭据配置、设备查询与终端选组 |
| [隧道与端口转发](docs/tunnels.zh-CN.md) | 连接 ADB 客户端、本地守护进程、断线重连与端口转发 |
| [服务端管理](docs/server.zh-CN.md) | USB 权限、systemd、自动分组、证书与配置重载 |
| [Docker 部署](docker/README.md) | Compose、USB 权限与凭据持久化 |
| [网关 API](docs/api.zh-CN.md) | 独占租约、REST 接口与 WebSocket 隧道协议 |
| [开发与发版](docs/development.zh-CN.md) | 测试、容器镜像与二进制发布 |

## 许可证

本项目采用 [The Unlicense](LICENSE)。
