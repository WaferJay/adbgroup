# 客户端指南

[项目首页](../README.zh-CN.md) · [English](client.md)

## 安装选项

在 macOS 或 Linux 上安装或升级到最新版本，见[快速安装](../README.zh-CN.md#安装-adg)。要固定版本或更改安装目录，可以分别传入 `--tag` 或 `--bin-dir`。

```sh
curl -fsSL https://raw.githubusercontent.com/WaferJay/adbgroup/main/install.sh | sh -s -- --tag v0.1.0-beta.4 --bin-dir "$HOME/bin"
```

手动安装时，选择与你的系统和架构匹配的压缩包，用 `checksums.txt` 校验后，将解压得到的 `adg` 放到 `PATH` 中。[下载发布包](https://github.com/WaferJay/adbgroup/releases)

## 安装 adg 技能

**安装到 Codex CLI**，在终端中运行以下命令。

```sh
codex plugin marketplace add WaferJay/adbgroup
codex plugin add adg@adbgroup
```

**安装到 Claude Code**，在 Claude Code 会话中运行以下命令。

```text
/plugin marketplace add WaferJay/adbgroup
/plugin install adg@adbgroup
```

开启新的智能体会话后，可以直接说「帮我安装 adg」，智能体会调用同一安装脚本。首次连接时，用 `adg profile import client.adgpack` 导入管理员提供的凭据包，然后说「用 adg 列出我能访问的设备组」。

## 导入和管理凭据

通过 SCP 等私密渠道，从管理员处接收 `client.adgpack`。包里含有用户的明文私钥。导入时建议的 profile 名来自签发输出目录名。

```sh
adg profile import client.adgpack
adg groups
adg profile import -name prod prod.adgpack
adg profile list
adg profile set-default prod
adg use -p alice team-a             # 当前终端选组，需要 shell 集成
adg -p prod team-a devices -l       # 单次使用 prod
adg profile update alice renewed.adgpack
```

新装客户端的首个 profile 自动成为默认，后续导入可以并存。续期后旧隧道和重连沿用原凭据版本。更换网关或身份需另取名称；更新服务端 CA 时会显示新旧 SHA-256 指纹并要求交互确认。本地存储位于 `~/.config/adbgroup/profiles/`，会为已有会话保留历史凭据版本。

签发时从 `pki.server_names` 选择实际服务端证书覆盖的名称，并使用 `api.listen` 的端口。未配置名称时使用证书中的具体 DNS 或 IP。只有一个候选时自动采用，有多个时导入命令会列出选项。脚本、没有候选或公网端口映射场景，可以在签发或导入时用 `-server https://host:port` 指定地址。HTTPS 服务使用独立 CA 时，签发或打包时加 `-server-ca gateway-ca.pem`。

在网关上签发或重新打包凭据，见[服务端证书](server.zh-CN.md#证书)。

### 已有的 YAML 配置

已有客户端继续读取 `~/.config/adbgroup/tunnel.yaml`，直到用户显式设置默认 profile。`-f` 可选用原有 YAML 配置，与 `-p` 互斥。旧配置示例：

```yaml
server: https://adb-host.example:8443
# 可省略；省略时默认为 tunnel.yaml 所在目录
credentials: /path/to/run/clients/alice
```

升级时，请删除已有客户端 YAML 中的顶层 `group:` 字段。可以在命令行上传入组名，或用 `adg use GROUP` 选择后续设备查询使用的组。

`credentials` 目录中的文件名使用 `cert` 命令的固定输出 `client.pem`、`client.key`、`ca.pem`。也可以把这三个文件直接放在 `tunnel.yaml` 旁边并省略 `credentials`。

如需将已有的 YAML 配置导入为具名 profile，可执行以下命令。

```sh
adg profile import -f staging.yaml -name staging
```

## 查询分组和设备

`groups/g` 和 `devices/d` 使用客户端配置及 mTLS 凭据，直接查询网关的 REST API。

```sh
adg g                  # 列出授权组名（NAME）及各组的设备数（DEVICES）
adg d                  # 当前组的设备、连接状态和锁状态
adg d team-b           # 指定组的设备，包括离线设备
adg g --json            # 组列表 JSON
adg d @devices --json  # 查询 devices 组的网关库存，输出 JSON
```

查询命令支持 `-s/-d/-f` 及对应长参数、`-cert/-key/-ca`，命令行覆盖配置文件。查询失败返回非零退出码，错误写入 stderr，JSON 结果单独写入 stdout。

## 按组运行 adb

安装 Android Platform Tools 并确保能从 PATH 找到 `adb` 后，把组名写在 adb 参数前即可。在新终端、脚本或 CI 中都可以直接调用，无需初始化 shell 或导出环境变量。

```sh
adg team-a devices -l                     # 列出 team-a 的设备
adg team-b -s USB_SERIAL_B shell          # 进入 team-b 中指定设备的 shell
adg -f staging.yaml team-a devices        # 指定客户端配置文件
adg @devices devices -l                   # 设备组本身名为 devices
```

客户端连接参数写在组名前。例如，组名前的 `-s` 指定网关服务端，组名后的 `-s` 指定 adb 设备序列号。首个参数优先匹配内置命令及别名。组名前可以加 `@`，遇到重名时用它显式选组。脚本中可写 `adg "@$group" devices -l`，变量值即使是命令或别名也会按组名处理。组名本身以 `@` 开头时再加一个 `@`，例如 `@@lab` 表示名为 `@lab` 的组。已有脚本仍可使用 `-g GROUP` 或 `-group GROUP`。

daemon 按服务端、凭据和组名复用普通隧道，首次创建时自动分配本地回环端口。单次调用使用自己的客户端配置和组名，即使终端已选中其他网关或组也是如此。原生 adb 接收原始参数、标准输入、标准输出和标准错误，退出码直接传回调用方。隧道已指定 ADB 服务端地址，与原生 adb 的服务端选项 `-H`、`-P`、`-L` 冲突。如需写租约，请使用 `adg t GROUP -w <lease-id>`。

## 为当前终端选组

启用 shell 集成后，`adg 组名` 是 `adg use 组名` 的简写，随后在该终端运行的 `adb` 命令使用选中的组。zsh 用户可将下面一行加入 `~/.zshrc`，并在当前终端执行一次。

```sh
eval "$(command adg shell-init zsh)"
```

bash 使用 `eval "$(command adg shell-init bash)"`，将它加入终端会加载的启动文件（通常为 `~/.bashrc`；登录 shell 需由 `~/.bash_profile` 加载该文件）。初始化不会自动修改这些文件，可以重复执行。

`adg devices` 查询当前终端选中的组。尚未选组或显式指定 `-f` 等连接参数时，请使用 `adg devices GROUP`。创建隧道始终需要显式指定组名。

```sh
adg use team-a         # 将当前终端切换到 team-a
adg devices --json     # 输出当前组的网关库存
adb devices -l         # 列出 team-a 的设备
adg team-b devices -l  # 临时查看 team-b 的设备
echo "$ADBGROUP_GROUP" # 仍是 team-a
adg @devices           # 切到名为 devices 的组，也可用 adg use @devices
adg                    # 与 adg use 打开同一选组菜单，按 Enter 或 q 取消
adg d                  # 查询当前选中组
adg g                  # 行首的 * 标记当前终端选中的组
adg unuse              # 恢复首次 use 前的 ADB_SERVER_SOCKET，保留 tunnel

# 首次也可以指定连接配置；后续 use/g/d 默认沿用当前选择
adg use team-a -s https://adb-host.example:8443 -d run/clients/alice
```

设备查询和隧道创建都会保留当前终端的选组状态。

`use` 按服务端、凭据路径和组复用普通 tunnel，也可复用 `t` 创建的匹配 tunnel；没有匹配项时由操作系统自动分配本地回环端口，不使用配置中的 `listen`。匹配多条时依次优先 active、starting、reconnecting、failed；同状态按 tunnel ID 选择。对 reconnecting tunnel 会等待恢复，不创建新监听；取消等待不会停止后台重连。failed tunnel 需显式重试。切换失败或取消会保留原选择。`use` 不选择写租约；写访问继续使用 `t GROUP -w`。

选择状态仅保存在当前 shell 环境，不修改 `tunnel.yaml`。随后从该终端启动的 adb/Appium 会继承 socket，已启动的进程不受影响。显式提供配置文件、服务端或凭据参数时，按原客户端配置规则解析，不套用当前选择。手动修改 `ADB_SERVER_SOCKET` 后，与其不匹配的选择上下文会被忽略。

`ADBGROUP_GROUP` 保存当前终端选中的组名，方便 shell 提示符和脚本使用。切换失败或取消时保留原值，`adg unuse` 时清除。升级后，在已打开的终端重新执行 shell 初始化命令，即可使用切组简写。

在尚未启用集成的终端中，`adg team-a` 和 `adg unuse` 会提示初始化方法。要直接运行一次 adb，可在组名后追加参数，例如 `adg team-a devices -l`。

### 等待设备

支持原生 `adb -s USB_SERIAL_A wait-for-device`：设备未连接、offline 或 unauthorized 时持续等待，进入 `device` 状态后返回；也可使用 `wait-for-recovery`、`wait-for-rescue`、`wait-for-sideload`、`wait-for-bootloader` 和 `wait-for-disconnect`。等待仅观察当前分组内的设备状态，不获取读锁，不受读锁等待超时限制；多设备时需用 `-s` 或 `-t` 指定目标。USB 的 `wait-for-disconnect` 在设备从列表消失时完成，单纯 offline 不算断开。Ctrl-C、隧道关闭或撤权会结束等待。

## 命令帮助

`adg --help`（也可用 `-h` 或 `-help`）列出命令。用 `adg <命令> -help` 查看参数和默认值，例如 `adg t -help`、`adg t stop -help` 或 `adg daemon -help`。运行帮助命令无需配置，也不会启动服务。

连接 Appium 或配置端口转发，见[隧道与端口转发](tunnels.zh-CN.md)。
