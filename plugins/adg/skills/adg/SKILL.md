---
name: adg
description: 通过 adg 访问 ADB 分组网关。用于导入客户端凭据、列出授权设备组、查询组内设备和锁状态，以及在指定组的 Android 设备上运行 adb 命令。
---

# adg

## 导入 .adgpack 凭据包

使用 `adg` v0.1.0-beta.4 或更高版本。管理员提供的 `.adgpack` 包含客户端证书和私钥。在运行 `adg` 的机器上，直接用本地文件路径导入。

```sh
adg profile import -name lab ~/Downloads/client.adgpack
adg profile list     # 核对配置名、网关、身份和证书到期时间
adg groups -p lab    # 用刚导入的凭据查询授权组，验证连接
```

`lab` 是本地配置名，省略 `-name lab` 时沿用包内名称。全新安装的首个配置自动成为默认。包中有多个网关地址时，可在终端按编号选择，或在文件路径前加 `-server URL` 指定 HTTPS 地址。

## 查询设备

- `adg groups` 列出当前身份可访问的组。
- `adg devices team-a` 查询该组的设备序列号、连接状态和读写锁。需要结构化结果时追加 `--json`。

## 执行 adb 命令

单次操作直接指定组和设备。将示例中的 `team-a` 和 `USB_SERIAL_A` 换成目标组和设备序列号。原生 `adb` 需在 `PATH` 中。

```sh
adg team-a -s USB_SERIAL_A shell getprop ro.product.model
```

`-p PROFILE` 等网关连接参数放在组名前，`-s SERIAL` 等 adb 参数放在组名后。组名与命令或别名重名时加 `@`，例如 `adg @devices devices -l`。

要在同一终端连续使用原生 `adb`，先运行 `eval "$(command adg shell-init zsh)"`（bash 用户把 `zsh` 换成 `bash`），再用 `adg use team-a` 选组。用完后运行 `adg unuse` 恢复原连接。
