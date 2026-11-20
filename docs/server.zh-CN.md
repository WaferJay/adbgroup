# 服务端管理

[项目首页](../README.zh-CN.md) · [English](server.md)

## 启动网关

在连接 USB 设备的 Linux 主机上安装 Android SDK Platform Tools，并给运行服务的账号授予 USB 设备访问权限。可以直接安装 Linux 版 `adg`，也可以在仓库根目录使用 Go 1.26+ 构建。

```sh
go build -o bin/adg ./cmd/adbgroup
export PATH="$PWD/bin:$PATH"
```

从仓库或解压后的发布包中复制配置示例。

```sh
cp config.example.yaml config.yaml
```

把 `backend.binary` 设为本机 `adb` 的路径，在 `groups` 中按 USB 序列号分组，再用 `identities` 把组授权给证书身份。首次启动前，在 `pki.server_names` 中填写客户端连接时使用的域名或 IP。[配置示例](../config.example.yaml)

```sh
adg check -config config.yaml
adg serve -config config.yaml
```

每个用户使用同一张客户端证书访问 API 和隧道。团队是网关配置中的虚拟分组，无需为其创建 Linux 账号、配置系统 sshd 或分发 SSH 公钥。

## USB 后端

REST API 和 WebSocket 隧道共用 HTTPS 入口。`backend.address` 使用数值回环地址（如
`127.0.0.1:5038`）时，ADB server 仅监听回环；使用裸端口（如 `5038`）时监听所有
接口，便于容器端口转发，网关内部仍通过 `127.0.0.1:5038` 连接。原始 ADB 端口不
经过 mTLS、分组授权和设备锁，只应显式映射到宿主机回环地址，不要向远程用户开放。

`backend.manage: true` 使用专用端口启动常驻 ADB server；端口已有进程时拒绝启动。单台设备拔插不重启后端，其他设备继续运行。后台 ADB server 故障仍会中断所有设备连接。

USB 枚举兼容原生后端的 `usb:` 路径和部分 libusb 版本不带前缀的总线/端口路径，继续排除网络设备和模拟器。

## 使用 systemd 或 Docker 部署

示例 systemd 单元以 `adbgroup` 身份运行 `/usr/local/bin/adg`，读取 `/etc/adbgroup/config.yaml`，状态目录为 `/var/lib/adbgroup`。在配置文件中设置 `pki.directory: /var/lib/adbgroup/pki`，并通过 udev 规则和适合发行版的 `plugdev` 组授予服务账号 USB 设备节点的访问权限。[systemd 单元](../deploy/adbgroup.service) · [udev 规则](../docker/70-android-adb.rules)

```sh
docker pull ghcr.io/waferjay/adbgroup:latest
```

Linux Docker 主机的 USB 权限、凭据持久化、Compose 启动和重载方式，见 [Docker 部署](../docker/README.md)。

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

### 签发与分发凭据包

使用托管 PKI 时，`adg serve` 每次启动都会为 `identities` 中新增的身份生成用户证书、私钥和 `client.adgpack`。例如，`spiffe://adbgroup/alice` 的包默认位于 `run/pki/clients/alice/client.adgpack`。通过私密渠道将包交给 Alice，客户端运行 `adg profile import client.adgpack` 即可。其他身份 URI 使用独立的目录名，首次签发日志会显示完整路径。

启动时，服务会用原有 PEM 文件补建缺失的包。如果包中的 HTTPS 候选地址有变化，服务会重新打包并原子替换旧包。修改 `api.listen` 端口后，重启服务并分发更新后的包。已导入旧地址的客户端可以用新名称导入，例如 `adg profile import -name alice-new client.adgpack`。

SIGHUP 重载时会更新授权，新增身份的凭据包会在服务下次启动时自动生成。使用外部 PKI 时，由管理员使用签发 CA 为用户签发凭据。

管理员也可以单独签发用户凭据：

```sh
adg cert -config config.yaml -user alice -out run/clients/alice
# 也可自定义身份与有效期
adg cert -config config.yaml \
  -identity spiffe://adbgroup/ci-runner -days 90 -out run/clients/ci-runner
```

输出目录包含 `client.adgpack` 和原有的 `client.pem`、`client.key`、`ca.pem`，不会导出 CA 私钥；已有输出目录会被拒绝。`-out` 相对命令当前目录，PKI 文件路径相对配置文件目录。CLI 可在首次启动前初始化默认 PKI，不启动监听或 ADB。

`-user alice` 对应 `spiffe://adbgroup/alice`。`identities` 的键须与客户端证书中的身份 URI 完全一致。配置检查、启动和重载都会拒绝使用 `urn:` 的身份。签发不会自动授予权限，把身份加入配置后发送 SIGHUP：

```yaml
identities:
  "spiffe://adbgroup/alice": [team-a]
```

同一 CA 新签发用户证书无需重启；服务端要求证书包含唯一 URI SAN，URI 必须出现在授权配置中。有效期允许 1–3650 天，默认 365 天，不超过签发 CA 的剩余期限。

企业 PKI 显式配置 API 的全部三个文件，签发时用 `-ca-key` 和可选的 `-ca-cert` 指定可信签发者。导出的 `ca.pem` 是客户端签发 CA 链；若 HTTPS 服务端由另一 CA 签发，隧道的 `-ca` 和 curl 的 `--cacert` 必须使用服务端 CA 信任文件。

要将已签发的 PEM 文件打包，可在网关上执行以下命令。

```sh
adg cert pack -config config.yaml -credentials run/clients/alice -bundle alice.adgpack
```

用户收到凭据包后即可在客户端导入。[导入和管理凭据](client.zh-CN.md#导入和管理凭据)

## 重载与迁移

SIGHUP 只热更新现有组的设备成员、身份授权、读等待时间和队列上限，允许调整组顺序。撤权关闭相关隧道/流并清理所属规则；共享设备上的其他有效用户连接继续使用。

所有真实监听端口、后端、TLS/PKI 和 `auto_groups` 配置变更必须冷重启；静态分组新增、删除、改名也沿用冷重启策略。重载遇到上述变更时拒绝整份新配置，保留旧配置，不部分应用。使用 `systemctl restart adbgroup` 显式冷重启，不保留旧代理连接、租约和网关规则。

这是不兼容升级：删除 `lease_ssh`、组 `listen`、`forward_min`、`forward_max` 和 `ssh-config` 命令；旧字段严格报错。删除不再使用的系统 SSH 组账号/限制片段由管理员在迁移后处理，程序不会自动修改系统。原有 CA 和客户端证书可继续使用，无需重签。服务仍使用一个守护进程 Linux 账号，不为用户或 team 创建账号。

可执行文件名为 `adg`；项目名、压缩包名称、配置路径、服务账号及证书身份继续使用
`adbgroup`。从原 `adbgroup` 可执行文件升级时，请将脚本和 shell 初始化中的调用改为 `adg`。
