# ADB 分组网关

[English](README.md) | **简体中文**

Linux USB 设备分组、统一 mTLS 身份认证、原生 ADB 访问和物理设备全局读写锁。每个用户持有自己的客户端证书，API 和隧道共用证书及授权配置。team 是虚拟分组，不需要 Linux 账号、系统 sshd 或 SSH 公钥。

```text
原生 adb / Appium → 本地 adbgroup tunnel → mTLS HTTPS → 分组与锁 → 常驻 ADB server → USB
```

## 构建与服务启动

需要 Go 1.26+、近期 Android Platform Tools 和 Linux USB/udev 权限。

```sh
go build -o bin/adbgroup ./cmd/adbgroup
cp config.example.yaml config.yaml
bin/adbgroup check -config config.yaml
bin/adbgroup serve -config config.yaml
```

不带参数运行会显示 usage；`adbgroup -help`（或 `-h`、`--help`）显示命令列表，
`adbgroup <命令> -help` 显示对应参数和默认值，例如 `adbgroup t -help`、
`adbgroup t stop -help`、`adbgroup daemon -help`。帮助命令返回成功，不读取配置或启动服务。

REST API 和 WebSocket 隧道共用 HTTPS 入口。`backend.address` 使用数值回环地址（如
`127.0.0.1:5038`）时，ADB server 仅监听回环；使用裸端口（如 `5038`）时监听所有
接口，便于容器端口转发，网关内部仍通过 `127.0.0.1:5038` 连接。原始 ADB 端口不
经过 mTLS、分组授权和设备锁，只应显式映射到宿主机回环地址，不要向远程用户开放。

`backend.manage: true` 使用专用端口启动常驻 ADB server；端口已有进程时拒绝启动。单台设备拔插不重启后端，其他设备继续运行。后台 ADB server 故障仍会中断所有设备连接。

USB 枚举兼容原生后端的 `usb:` 路径和部分 libusb 版本不带前缀的总线/端口路径，继续排除网络设备和模拟器。

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
bin/adbgroup cert -config config.yaml -user alice -out run/clients/alice
# 也可自定义身份与有效期
bin/adbgroup cert -config config.yaml \
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

## 普通客户端与透明转发

客户端安装同一二进制，启动本地隧道；这个命令不读取服务端 config.yaml。可参考 `tunnel.example.yaml`，客户端默认读取固定位置 `~/.config/adbgroup/tunnel.yaml`：

```yaml
server: https://adb-host.example:8443
group: team-a
# 可省略；省略时默认为 tunnel.yaml 所在目录
credentials: /path/to/run/clients/alice
```

`credentials` 目录中的文件名使用 `cert` 命令的固定输出 `client.pem`、`client.key`、`ca.pem`。也可以把这三个文件直接放在 `tunnel.yaml` 旁边并省略 `credentials`。配置完成后启动命令只有：

```sh
adbgroup t
```

`t` 默认自动启动当前用户的后台 daemon，将 tunnel 交给它管理，等本地监听就绪后返回。关闭终端后 tunnel 继续运行。同一 daemon 可以管理多个服务端或分组的 tunnel，每条 tunnel 需要使用不同的本地监听端口：

```sh
adbgroup t -g team-a -l 15037
adbgroup t -g team-b -l 15038
adbgroup t list             # ID、服务端、分组、监听地址、状态、最近错误
adbgroup t list --json
adbgroup t stop <id>        # 停止并移除指定 tunnel

adbgroup daemon status
adbgroup daemon start      # 可选；t 命令会自动启动
adbgroup daemon stop       # 等待全部 tunnel 清理后退出

adbgroup t --no-daemon      # 在当前终端前台运行，Ctrl-C 退出
```

启动成功会输出 tunnel ID 和 `export ADB_SERVER_SOCKET=tcp:127.0.0.1:端口`。在使用 adb/Appium 的终端设置对应的环境变量即可选择 tunnel。重复提交相同参数会复用已有 tunnel；同一监听地址使用不同配置会报冲突，需要先停止旧 tunnel。

连接断开时只影响该 tunnel，其状态变为 `failed`，原有 forward/reverse 规则清空；再次提交相同启动命令可手动恢复，规则需要重新创建。daemon 不自动重连、不保存 tunnel 配置，重启后需要重新提交启动命令。

daemon 支持 macOS/Linux，控制 socket 位于 `~/.config/adbgroup/daemon/control.sock`，目录权限为 `0700`，socket 权限为 `0600`；日志写入同目录的 `daemon.log`。本地控制协议版本不兼容时会报错，应先使用旧版本命令停止 daemon，再使用新版本启动。首版不安装系统服务或配置开机自启。

不使用配置文件时，远程连接也可以简写为：

```sh
adbgroup t -s https://adb-host.example:8443 -d run/clients/alice

# 另一终端
export ADB_SERVER_SOCKET=tcp:127.0.0.1:15037
adb devices -l
adb -s USB_SERIAL_A shell
adb -s USB_SERIAL_A push app.apk /data/local/tmp/
adb -s USB_SERIAL_A install app.apk
```

`t` 是 `tunnel` 的短名，`-s/-g/-l/-d/-w/-f` 分别是 `-server/-group/-listen/-credentials/-lease/-config`。命令行参数优先于配置文件。没有默认配置文件时，内置默认服务为 `https://127.0.0.1:8443`，默认凭据目录为当前目录，默认本地监听为 `127.0.0.1:15037`。未配置分组且证书身份只授权一个组时会自动选择；身份授权多个组时必须用 `-g` 选择。原有长参数及单独的 `-cert/-key/-ca` 均继续兼容并覆盖目录默认值。

`-l 15038` 等价于 `-l 127.0.0.1:15038`，配置文件同样支持 `listen: 15038`；完整 IPv4/IPv6 数值回环地址也可继续使用。

`groups/g` 和 `devices/d` 直接查询 REST API，使用相同的默认客户端配置及 mTLS 凭据，无需启动隧道：

```sh
adbgroup g                  # 所有授权组、设备数量，* 标记默认组
adbgroup d                  # 默认组的设备、连接状态和锁状态
adbgroup d -g team-b         # 指定组的设备，包括离线设备
adbgroup g --json            # 组列表 JSON
adbgroup d -g team-b --json  # 设备列表 JSON，便于脚本处理
```

查询命令支持 `-s/-g/-d/-f` 及对应长参数、`-cert/-key/-ca`，命令行覆盖配置文件。设备查询未指定或配置分组时，只在有且仅有一个授权组的情况下自动选择；多个组时需用 `-g` 指定。查询失败返回非零退出码，错误写入 stderr，JSON 结果单独写入 stdout。

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

监听规则本身不持锁，每条实际数据连接持有对应读锁或租约访问资格。断线、Ctrl-C、撤权、证书到期后关闭隧道和所属规则；重新运行隧道命令创建新会话，需要重新执行 forward/reverse。不自动重连、恢复规则或重放 ADB 命令。

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
adbgroup t -s https://adb-host.example:8443 -d run/clients/alice \
  -g team-a -w LEASE_ID -l 15039
adb -H 127.0.0.1 -P 15039 -s USB_SERIAL_A shell
```

只有同一证书身份、同一分组、有效且 active 的租约可建立写隧道，并只能访问绑定设备。普通隧道不会自动升级。用户可用同一身份的新证书重连尚未到期的租约；隧道断线不自动释放租约，TTL 或 DELETE 决定其生命周期。

写锁 FIFO 等待已有读连接排空，不抢占已有流；等待默认 30 秒，允许 1–3600 秒；租约从 active 开始计时，TTL 默认 300 秒，允许 1–1800 秒。`PATCH {"expires_at":"RFC3339"}` 精确设置到期时间，必须在未来且不超过当前时间 30 分钟；仅尚未到期的 active 租约可更新。DELETE 取消排队或释放租约，重复调用幂等。

设备拔出使该设备的会话和租约失效，并清除其 forward/reverse 规则及本地监听；其他设备的规则和连接继续使用。重新插入按序列号恢复成员归属，转发规则不会自动恢复，需要用户或工具重新执行命令。新建 forward 会等待旧监听清理完成，并创建独立于旧设备代次的监听器；旧 transport ID、旧租约和旧规则不能复用。重复序列号拒绝访问。

## REST 与隧道接口

以下接口全部要求 mTLS；路径参数按 URL 路径段编码。分组配置由 YAML 管理，API 不提供分组成员写接口。

| 方法与路径 | 含义 |
| --- | --- |
| GET /v1/groups | 列出授权组，按配置顺序 |
| GET /v1/groups/{group} | 组名及配置设备列表 |
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

所有真实监听端口、后端、TLS/PKI 配置变更必须冷重启；分组新增、删除、改名也沿用冷重启策略。重载遇到上述变更时拒绝整份新配置，保留旧配置，不部分应用。使用 `systemctl restart adbgroup` 显式冷重启，不保留旧代理连接、租约和网关规则。

这是不兼容升级：删除 `lease_ssh`、组 `listen`、`forward_min`、`forward_max` 和 `ssh-config` 命令；旧字段严格报错。删除不再使用的系统 SSH 组账号/限制片段由管理员在迁移后处理，程序不会自动修改系统。原有 CA 和客户端证书可继续使用，无需重签。服务仍使用一个守护进程 Linux 账号，不为用户或 team 创建账号。

部署示例 `deploy/adbgroup.service` 使用服务账号 adbgroup，状态目录 /var/lib/adbgroup。配置放在 /etc/adbgroup/config.yaml，设置 `pki.directory: /var/lib/adbgroup/pki`。配置 USB udev 权限及适合发行版的 plugdev 组。

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

端到端测试用内存连接运行真实 mTLS、HTTP Upgrade 和 WebSocket，覆盖 ADB 数据、客户端 forward/reverse、身份撤销、租约绑定和断线清理；不要求 OS TCP 监听权限。原生 adb 测试需要安装 adb 且允许 TCP 监听，否则跳过。真实 USB 验收需共享设备、同组两设备及另组设备，并发 shell、文件传输和转发后拔插单台，确认其他设备连接不中断。

无外部依赖时可使用 Go 1.22+ 单独验证核心和 PKI：

```sh
GOTOOLCHAIN=local go test -modfile=test/core.mod -race ./internal/gateway ./internal/pki
```

macOS 新系统配旧 Go 1.22 若遇 missing LC_UUID，可加 `-ldflags=-linkmode=external`。生产构建及完整验收使用 Go 1.26+。
