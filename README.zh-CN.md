# ADB 分组网关

[English](README.md) | **简体中文**

Linux USB 设备分组、统一 mTLS 身份认证、原生 ADB 访问和物理设备全局读写锁。每个用户持有自己的客户端证书，API 和隧道共用证书及授权配置。team 是虚拟分组，不需要 Linux 账号、系统 sshd 或 SSH 公钥。

```text
原生 adb / Appium → 本地 adbgroup tunnel → mTLS HTTPS → 分组与锁 → 常驻 ADB server → USB
```

## 构建与服务启动

需要 Go 1.26+、近期 Android Platform Tools 和 Linux USB/udev 权限。

```sh
go build -o bin/adg ./cmd/adbgroup
cp config.example.yaml config.yaml
bin/adg check -config config.yaml
bin/adg serve -config config.yaml
```

`adg --help`（或 `-h`、`-help`）显示命令列表。在已启用 shell 集成的交互终端中，
直接运行 `adg` 和运行 `adg use` 都会打开选组菜单。
`adg <命令> -help` 显示对应参数和默认值，例如 `adg t -help`、
`adg t stop -help`、`adg daemon -help`。帮助命令返回成功，不读取配置或启动服务。

REST API 和 WebSocket 隧道共用 HTTPS 入口。`backend.address` 使用数值回环地址（如
`127.0.0.1:5038`）时，ADB server 仅监听回环；使用裸端口（如 `5038`）时监听所有
接口，便于容器端口转发，网关内部仍通过 `127.0.0.1:5038` 连接。原始 ADB 端口不
经过 mTLS、分组授权和设备锁，只应显式映射到宿主机回环地址，不要向远程用户开放。

`backend.manage: true` 使用专用端口启动常驻 ADB server；端口已有进程时拒绝启动。单台设备拔插不重启后端，其他设备继续运行。后台 ADB server 故障仍会中断所有设备连接。

USB 枚举兼容原生后端的 `usb:` 路径和部分 libusb 版本不带前缀的总线/端口路径，继续排除网络设备和模拟器。

## Docker 镜像

[Docker 镜像工作流](.github/workflows/docker.yml) 构建 `linux/amd64` 和
`linux/arm64` 镜像。面向 `main` 的 PR 仅验证构建；推送到 `main`、推送 `v*`
标签或手动运行工作流时，使用内置 `GITHUB_TOKEN` 发布到
`ghcr.io/waferjay/adbgroup`，无需额外配置镜像仓库密钥。

构建镜像前，工作流使用 Go 1.26 的最新补丁版本运行 `go vet ./...` 和
`go test -race -count=1 -timeout=5m ./...`，并安装 ADB 执行原生客户端兼容性测试，
安装 bash、zsh 和 util-linux 提供的 `script` 执行 shell 集成测试。检查失败时不会构建或发布镜像。

`latest` 和 `main` 镜像标签跟随 `main` 分支；`v1.0.0` 等 Git 标签生成同名镜像
标签，每次发布还会生成 `sha-<短提交号>` 标签。

```sh
docker pull ghcr.io/waferjay/adbgroup:latest
```

配置、USB 权限和 Compose 命令见 [Docker 部署](docker/README.md)。首次发布后，
如需匿名拉取镜像，请将 GHCR 包的可见性设为 public。

## 二进制发版

推送 `v0.1.0` 或 `v0.1.0-beta.1` 等语义化版本标签会触发
[CI 与发版工作流](.github/workflows/docker.yml)。测试和 Docker 镜像发布成功后，
[发版任务](.github/workflows/release.yml) 使用 [GoReleaser](.goreleaser.yaml)
创建 GitHub Release，包含：

- Linux、macOS 的 `amd64`、`arm64` 二进制压缩包，命名为
  `adbgroup_<版本>_<系统>_<架构>.tar.gz`；
- SHA-256 校验文件 `checksums.txt`；
- 根据上一标签之后的提交生成的发布说明。

每个压缩包附带中英文 README、服务端和客户端配置示例、systemd 服务及 Linux
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

产物位于 `dist/`。从 [Releases 页面](https://github.com/WaferJay/adbgroup/releases)
下载对应压缩包、验证校验和后，将解压得到的 `adg` 放入 PATH 即可。

可执行文件名为 `adg`；项目名、压缩包名称、配置路径、服务账号及证书身份继续使用
`adbgroup`。从原 `adbgroup` 可执行文件升级时，请将脚本和 shell 初始化中的调用改为 `adg`。

## 证书


省略 `api.certificate`、`api.key`、`api.client_ca` 三项时，首次启动自动在 `pki.directory`（默认配置目录下的 `run/pki`）生成 CA 和服务端凭据，后续复用。三项只能全部省略或全部指定。

```yaml
api:
  listen: 0.0.0.0:8443
pki:
  directory: run/pki
  server_names: [adb-host.example, 192.168.1.10]
```

生成文件为 `ca.pem`、`ca.key`、`server.pem`、`server.key`；目录权限 0700、文件 0600。CA 默认有效 10 年，服务端证书默认 2 年且不超过 CA 到期时间。SAN 包含 localhost、回环 IP、主机名和当前本机接口 IP；首次初始化前用 `server_names` 补充公网或 NAT 域名/IP。

不会因为重复启动、网络变化或新增 SAN 自动覆盖证书。凭据部分缺失、损坏或过期会报错，需管理员恢复或更新。替换服务端证书或 CA 信任文件后需要冷重启。

管理员签发用户凭据：

```sh
bin/adg cert -config config.yaml -user alice -out run/clients/alice
# 也可自定义身份与有效期
bin/adg cert -config config.yaml \
  -identity spiffe://adbgroup/ci-runner -days 90 -out run/clients/ci-runner
```

输出目录包含 `client.pem`、`client.key`、`ca.pem`，不会导出 CA 私钥；已有输出目录会被拒绝。`-out` 相对命令当前目录，PKI 文件路径相对配置文件目录。CLI 可在首次启动前初始化默认 PKI，不启动监听或 ADB。

`-user alice` 对应 `spiffe://adbgroup/alice`。签发不会自动授予权限，把身份加入配置后发送 SIGHUP：

```yaml
identities:
  "spiffe://adbgroup/alice": [team-a]
```

同一 CA 新签发用户证书无需重启；服务端要求证书包含唯一 URI SAN，URI 必须出现在授权配置中。有效期允许 1–3650 天，默认 365 天，不超过签发 CA 的剩余期限。

企业 PKI 显式配置 API 的全部三个文件，签发时用 `-ca-key` 和可选的 `-ca-cert` 指定可信签发者。导出的 `ca.pem` 是客户端签发 CA 链；若 HTTPS 服务端由另一 CA 签发，隧道的 `-ca` 和 curl 的 `--cacert` 必须使用服务端 CA 信任文件。

## 自动分组与通配授权

服务端可按设备 serial 或 ADB `model` 自动分组，各配置块省略时默认关闭：

```yaml
auto_groups:
  all:
    enabled: true
    name: all-devices
  configured:
    enabled: true
    name: static-devices
  discovery:
    enabled: true
    name: discovered-devices
  serial:
    enabled: true
    source: all-devices
    inherit_access: true
  model:
    enabled: true
    source: static-devices
    prefix: model-

identities:
  "spiffe://adbgroup/admin": ["*"]
  "spiffe://adbgroup/alice": [discovered-devices]
  "spiffe://adbgroup/bob": [model-Pixel_8]
```

`configured` 汇总所有静态 `groups` 的设备，`discovery` 纳入当前 USB 监控发现的设备并汇总其中未配置的设备，`all` 包含两者的并集。三个汇总组的 `name` 默认分别为 `configured`、`discovery`、`all`。单独启用 `all` 不会开启发现，也不要求启用 `configured` 汇总组。动态设备断开后在内存中保留，重启后重新发现，无需状态文件；网络设备和模拟器不纳入动态发现。

serial、model 的 `source` 必填，引用最终汇总组名或一个静态组；不能引用其他生成组。serial 为每台设备生成以原始 serial 命名的单设备组，可用 `adg use USB_SERIAL_A` 选择。model 仅对输入组按 ADB 原始 `model` 值归类，不执行设备 shell 命令；默认前缀为 `model-`。首次未知机型暂不归类，进程内保留最后已知机型，新非空值更新归属。

`inherit_access` 默认关闭。开启后，用户通过显式授权的静态组、汇总组或机型组获得设备权限，也可访问对应 serial 组；serial 权限不会反向授予机型组或其他汇总组权限。例如 Bob 获得 Pixel_8 机型组权限后，也能选择其设备的 serial 组。汇总组授权自然覆盖未来加入的设备。可以预先授权尚未出现的 serial 或机型组，但必须启用对应规则。

identities 中的完整条目 `"*"` 授权所有现有及未来分组，包括静态组和自动组；不支持 `model-*` 等部分匹配。通配授权仍要求组存在、设备可用和有效的写租约，并遵守物理设备锁。必须引用 YAML 的 `"*"`。有多个授权组时，可用 `adg 组名 <adb 参数>` 指定本次调用的组，也可在启用 shell 集成后用 `adg 组名` 切换当前终端的组。

所有组共享名称空间。配置可确定的名称冲突会拒绝启动或重载；运行时冲突会记录错误并拒绝发布冲突的生成组，保留静态组与汇总组。自动分组配置变化需要重启。静态成员、身份或机型变化会更新派生组及权限，关闭失效访问并撤销相关租约。

组列表先按配置顺序返回静态组，再返回 configured、discovery、all 汇总组，最后分别按名称排序返回 serial、model 组。自动组成员按 serial 排序去重，断开的成员仍可查询。

## 普通客户端与透明转发

客户端安装同一二进制，启动本地隧道；这个命令不读取服务端 config.yaml。可参考 `tunnel.example.yaml`，客户端默认读取固定位置 `~/.config/adbgroup/tunnel.yaml`：

```yaml
server: https://adb-host.example:8443
# 可省略；省略时默认为 tunnel.yaml 所在目录
credentials: /path/to/run/clients/alice
```

升级时，请删除已有客户端 YAML 中的顶层 `group:` 字段。可以在命令行上传入组名，或用 `adg use GROUP` 选择后续设备查询使用的组。

`credentials` 目录中的文件名使用 `cert` 命令的固定输出 `client.pem`、`client.key`、`ca.pem`。也可以把这三个文件直接放在 `tunnel.yaml` 旁边并省略 `credentials`。

### 按组运行 adb

安装 Android Platform Tools 并确保能从 PATH 找到 `adb` 后，把组名写在 adb 参数前即可。在新终端、脚本或 CI 中都可以直接调用，无需初始化 shell 或导出环境变量。

```sh
adg team-a devices -l                     # 列出 team-a 的设备
adg team-b -s USB_SERIAL_B shell          # 进入 team-b 中指定设备的 shell
adg -f staging.yaml team-a devices        # 指定客户端配置文件
adg @devices devices -l                   # 设备组本身名为 devices
```

客户端连接参数写在组名前。例如，组名前的 `-s` 指定网关服务端，组名后的 `-s` 指定 adb 设备序列号。首个参数优先匹配内置命令及别名。组名前可以加 `@`，遇到重名时用它显式选组。脚本中可写 `adg "@$group" devices -l`，变量值即使是命令或别名也会按组名处理。组名本身以 `@` 开头时再加一个 `@`，例如 `@@lab` 表示名为 `@lab` 的组。已有脚本仍可使用 `-g GROUP` 或 `-group GROUP`。

daemon 按服务端、凭据和组名复用普通隧道，首次创建时自动分配本地回环端口。单次调用使用自己的客户端配置和组名，即使终端已选中其他网关或组也是如此。原生 adb 接收原始参数、标准输入、标准输出和标准错误，退出码直接传回调用方。隧道已指定 ADB 服务端地址，与原生 adb 的服务端选项 `-H`、`-P`、`-L` 冲突。如需写租约，请使用 `adg t GROUP -w <lease-id>`。

### 启动本地隧道

要为 adb 或 Appium 等 ADB 客户端启动隧道，可运行以下命令。

```sh
adg t team-a
```

`t` 默认自动启动当前用户的后台 daemon，将 tunnel 交给它管理，等本地监听就绪后返回。关闭终端后 tunnel 继续运行。同一 daemon 可以管理多个服务端或分组的 tunnel，每条 tunnel 需要使用不同的本地监听端口：

```sh
adg t team-a -l 15037
adg t team-b -l 15038
adg t @list -l 15040   # 为名为 list 的组创建隧道
adg t list             # ID、服务端、分组、监听地址、状态、最近错误
adg t list --json
adg t stop <id>        # 停止并移除指定 tunnel

adg daemon status
adg daemon start      # 可选；t 命令会自动启动
adg daemon stop       # 等待全部 tunnel 清理后退出

adg t team-a --no-daemon      # 在当前终端前台运行，Ctrl-C 退出
```

启动成功会输出 tunnel ID 和 `export ADB_SERVER_SOCKET=tcp:127.0.0.1:端口`。在使用 adb/Appium 的终端设置对应的环境变量即可选择 tunnel。重复提交相同参数会复用已有 tunnel；同一监听地址使用不同配置会报冲突，需要先停止旧 tunnel。

### 为当前终端选组

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

升级到 daemon 控制协议 v3 前，请用旧版本 `adg` 执行 `adg daemon stop`。升级后，用新版本重新提交 tunnel。

普通托管 tunnel 首次连接成功后，server 重启或网络断开会使其进入 `reconnecting`。daemon 保留原本地端口、分组和本地 tunnel ID，重新读取凭据并建立新的认证会话。不可用期间新 ADB 请求立即返回 `FAIL`，已有连接结束；恢复后后续命令通过原 socket 即可访问，无需再次 `use`。forward/reverse 规则需要重新创建，ADB 命令不会重放。

重连采用从 1 秒增长至 30 秒的指数退避，带 ±20% 抖动；持续在线 30 秒后重置，每轮连接限时 30 秒。网络故障、HTTP 5xx/429/408 和暂时缺失的分组（404）会继续重试。证书、信任校验、协议或明确身份拒绝会停止自动重试并显示 `failed`，本地端口仍保留至停止 tunnel；修复后重复 `use` 或 `t` 可显式重试。首次连接失败、前台 `t GROUP --no-daemon` 和 `t GROUP -w` 写租约隧道仍需显式重试。daemon 不持久化 tunnel 配置，daemon 自身重启后需重新提交。

daemon 支持 macOS/Linux，控制 socket 位于 `~/.config/adbgroup/daemon/control.sock`，目录权限为 `0700`，socket 权限为 `0600`；日志写入同目录的 `daemon.log`。本地控制协议版本不兼容时会报错，应先使用旧版本命令停止 daemon，再使用新版本启动。首版不安装系统服务或配置开机自启。

不使用配置文件时，远程连接也可以简写为：

```sh
adg t -s https://adb-host.example:8443 -d run/clients/alice team-a

# 另一终端
export ADB_SERVER_SOCKET=tcp:127.0.0.1:15037
adb devices -l
adb -s USB_SERIAL_A shell
adb -s USB_SERIAL_A push app.apk /data/local/tmp/
adb -s USB_SERIAL_A install app.apk
```

`t` 是 `tunnel` 的短名，`-s/-l/-d/-w/-f` 分别是 `-server/-listen/-credentials/-lease/-config`。命令行参数优先于配置文件。没有默认配置文件时，内置默认服务为 `https://127.0.0.1:8443`，默认凭据目录为当前目录，默认本地监听为 `127.0.0.1:15037`。单独的 `-cert`、`-key`、`-ca` 参数可覆盖凭据目录默认值。

`-l 15038` 等价于 `-l 127.0.0.1:15038`，配置文件同样支持 `listen: 15038`；完整 IPv4/IPv6 数值回环地址也可继续使用。

`groups/g` 和 `devices/d` 使用客户端配置及 mTLS 凭据，直接查询网关的 REST API。

```sh
adg g                  # 列出授权组名（NAME）及各组的设备数（DEVICES）
adg d                  # 当前组的设备、连接状态和锁状态
adg d team-b           # 指定组的设备，包括离线设备
adg g --json            # 组列表 JSON
adg d @devices --json  # 查询 devices 组的网关库存，输出 JSON
```

查询命令支持 `-s/-d/-f` 及对应长参数、`-cert/-key/-ca`，命令行覆盖配置文件。查询失败返回非零退出码，错误写入 stderr，JSON 结果单独写入 stdout。

隧道先完成远端认证，再监听本地端口；只接受数值回环地址。本地端口是客户端的可信边界，同机能连接该端口的进程可使用它。服务端会根据证书身份和所选组再次授权每个数据通道，不使用端口号、来源 IP 或隧道 ID 代替身份。

普通设备连接自动获取读锁，结束时释放。读锁表示并发调度资格，允许运行会修改设备的命令；多连接命令不保证整体原子性。写锁等待或生效期间，新普通操作默认等待最多 30 秒，最多排队 128 个请求；长时间 logcat 等现有流可能使写锁申请超时。

支持原生 `adb -s USB_SERIAL_A wait-for-device`：设备未连接、offline 或 unauthorized 时持续等待，进入 `device` 状态后返回；也可使用 `wait-for-recovery`、`wait-for-rescue`、`wait-for-sideload`、`wait-for-bootloader` 和 `wait-for-disconnect`。等待仅观察当前分组内的设备状态，不获取读锁，不受读锁等待超时限制；多设备时需用 `-s` 或 `-t` 指定目标。USB 的 `wait-for-disconnect` 在设备从列表消失时完成，单纯 offline 不算断开。Ctrl-C、隧道关闭或撤权会结束等待。

forward/reverse 的主机端现在都位于用户电脑：

```sh
adb -s USB_SERIAL_A forward tcp:8080 tcp:80
# 访问用户电脑 127.0.0.1:8080，即访问设备 TCP 80
adb -s USB_SERIAL_A forward tcp:0 localabstract:example
adb forward --list
adb -s USB_SERIAL_A forward --remove tcp:8080

# 用户电脑在 18080 运行应用；设备访问自身 8080 时连接该应用
adb -s USB_SERIAL_A reverse tcp:8080 tcp:18080
adb -s USB_SERIAL_A reverse --list
adb -s USB_SERIAL_A reverse --remove-all
```

forward 规则按隧道隔离，客户端监听成功后才返回 ADB 成功响应；`tcp:0` 使用客户端 OS 分配的端口。相同电脑上的实际绑定冲突会报错，默认可重绑同一设备，`--no-rebind` 拒绝覆盖。设备目标支持 TCP、localabstract 和 JDWP。

reverse 支持 TCP 设备监听及 TCP 客户端回环目标。设备端口由物理设备共享，有权访问该设备的其他组可以列出、删除或重绑规则；数据目标始终绑定创建规则的隧道。旧隧道或旧租约清理不会删除已被其他隧道覆盖的新规则。

网关与本机 ADB server 之间通过每条规则唯一的 Linux abstract Unix socket 回连，名称在规则删除及服务重启后均不复用。设备端残留的 reverse 映射因此不会因 TCP 端口复用而进入其他会话。删除失败返回 ADB FAIL、停止新接入并后台重试；若删除已生效但响应丢失，会查询设备端规则确认结果。

监听规则本身不持锁，每条实际数据连接持有对应读锁或租约访问资格。断线、Ctrl-C、撤权、证书到期后关闭远端会话和所属规则；普通托管 tunnel 在可恢复故障后自动建立新会话，保留主 ADB 本地端口。forward/reverse 规则不自动恢复，ADB 命令不重放。

撤权或租约结束会取消后端 I/O，并等待请求处理结束、后端连接关闭后再交接设备锁。设备代次仍有效时，失败的 reverse 清理会保留状态重试；代次变化后只回收旧入口，避免误删新设备会话的映射。

## 独占租约

通过同一证书申请物理设备全局写锁：

```sh
curl --cacert run/clients/alice/ca.pem \
  --cert run/clients/alice/client.pem --key run/clients/alice/client.key \
  -H 'Content-Type: application/json' \
  -d '{"wait_seconds":30,"ttl_seconds":300}' \
  https://adb-host.example:8443/v1/groups/team-a/devices/USB_SERIAL_A/write-leases
```

返回 `201`、规范嵌套资源 `Location` 和 `{"lease": ...}`，不返回密码或 SSH 凭据。轮询 Location，等待 active 后建立显式写隧道：

```sh
adg t -s https://adb-host.example:8443 -d run/clients/alice \
  team-a -w LEASE_ID -l 15039
adb -H 127.0.0.1 -P 15039 -s USB_SERIAL_A shell
```

只有同一证书身份、同一分组、有效且 active 的租约可建立写隧道，并只能访问绑定设备。普通隧道不会自动升级。用户可用同一身份的新证书重连尚未到期的租约；隧道断线不自动释放租约，TTL 或 DELETE 决定其生命周期。

写锁 FIFO 等待已有读连接排空，不抢占已有流；等待默认 30 秒，允许 1–3600 秒；租约从 active 开始计时，TTL 默认 300 秒，允许 1–1800 秒。`PATCH {"expires_at":"RFC3339"}` 精确设置到期时间，必须在未来且不超过当前时间 30 分钟；仅尚未到期的 active 租约可更新。DELETE 取消排队或释放租约，重复调用幂等。

设备拔出使该设备的会话和租约失效，并清除其 forward/reverse 规则及本地监听；其他设备的规则和连接继续使用。重新插入按序列号恢复成员归属，转发规则不会自动恢复，需要用户或工具重新执行命令。新建 forward 会等待旧监听清理完成，并创建独立于旧设备代次的监听器；旧 transport ID、旧租约和旧规则不能复用。重复序列号拒绝访问。

## REST 与隧道接口

以下接口全部要求 mTLS；路径参数按 URL 路径段编码。分组配置由 YAML 管理，API 不提供分组成员写接口。

| 方法与路径 | 含义 |
| --- | --- |
| GET /v1/groups | 列出授权组，静态组在前，自动组随后 |
| GET /v1/groups/{group} | 组名及有效设备成员列表 |
| GET /v1/groups/{group}/devices | 组设备状态及锁状态，包括断开的成员 |
| GET /v1/groups/{group}/devices/{device} | 单设备状态和 mTLS 隧道入口描述 |
| POST /v1/groups/{group}/devices/{device}/write-leases | 正文含可选 wait_seconds、ttl_seconds |
| GET /v1/groups/{group}/devices/{device}/write-leases/{lease} | 查询自己的租约 |
| PATCH /v1/groups/{group}/devices/{device}/write-leases/{lease} | 设置 expires_at |
| DELETE /v1/groups/{group}/devices/{device}/write-leases/{lease} | 幂等释放 |
| POST /v1/groups/{group}/tunnels | 正文 {} 或 {"lease_id":"..."}；创建 pending 隧道 |
| GET /v1/groups/{group}/tunnels/{tunnel} | 查询自己的 pending/connecting/active/closed 隧道 |
| DELETE /v1/groups/{group}/tunnels/{tunnel} | 幂等关闭，终态保留一小时 |
| GET /v1/groups/{group}/tunnels/{tunnel}/control | 升级为 WebSocket 控制连接 |
| POST /v1/groups/{group}/tunnels/{tunnel}/streams | 正文 {"kind":"adb"} 或 {"kind":"forward","rule_id":"..."} |
| GET /v1/groups/{group}/tunnels/{tunnel}/streams/{stream} | 升级为单次接入的二进制 WebSocket 数据流 |

隧道和数据流返回 `id`、`path`，创建成功同时返回 Location。ID 不代表授权；资源绑定身份、分组、隧道和用途。无权限或不存在的资源返回 404，未授权身份返回 403，状态冲突/流上限返回 409，隧道数量超限返回 429。正文最多 4 KiB，拒绝未知字段和多个 JSON 值。

控制消息是版本 1 JSON：`version,id,op,rule,previous,stream,port,error`。服务端 ready 后，客户端处理 bind、activate、remove、remember、forget、reverse，以同一 id 回复 reply；转发绑定与激活分两阶段，监听失败不覆盖既有规则。reverse 数据流由服务端创建，通过 control 告知所属客户端，仅允许接入已确认规则的回环目标。

每条数据 WebSocket 使用二进制消息，无压缩；每个本地 ADB 或转发连接独占一条数据 WebSocket，控制消息独立传输。客户端 WebSocket 使用 HTTP/1.1，反向代理需支持 WebSocket Upgrade 和长连接，且必须保留端到端 mTLS 身份认证；不信任代理传入的身份 Header。

隧道/数据流须在创建后 10 秒内接入；控制消息上限 64 KiB，待处理请求上限 64，响应超时 10 秒，心跳每 15 秒一次、等待 10 秒。每隧道最多 128 条数据流，每身份最多 32 个未关闭隧道。长数据流无固定总时限，仍受证书、权限、设备代次和租约约束。

## 重载、迁移与验证

SIGHUP 只热更新现有组的设备成员、身份授权、读等待时间和队列上限，允许调整组顺序。撤权关闭相关隧道/流并清理所属规则；共享设备上的其他有效用户连接继续使用。

所有真实监听端口、后端、TLS/PKI 和 `auto_groups` 配置变更必须冷重启；静态分组新增、删除、改名也沿用冷重启策略。重载遇到上述变更时拒绝整份新配置，保留旧配置，不部分应用。使用 `systemctl restart adbgroup` 显式冷重启，不保留旧代理连接、租约和网关规则。

这是不兼容升级：删除 `lease_ssh`、组 `listen`、`forward_min`、`forward_max` 和 `ssh-config` 命令；旧字段严格报错。删除不再使用的系统 SSH 组账号/限制片段由管理员在迁移后处理，程序不会自动修改系统。原有 CA 和客户端证书可继续使用，无需重签。服务仍使用一个守护进程 Linux 账号，不为用户或 team 创建账号。

部署示例 `deploy/adbgroup.service` 使用服务账号 adbgroup，状态目录 /var/lib/adbgroup。配置放在 /etc/adbgroup/config.yaml，设置 `pki.directory: /var/lib/adbgroup/pki`。配置 USB udev 权限及适合发行版的 plugdev 组。

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
