# 隧道与端口转发

[项目首页](../README.zh-CN.md) · [English](tunnels.md)

```text
原生 adb / Appium → 本地 adg 隧道 → HTTPS (mTLS) → Linux 网关 → ADB server → USB 设备
```

## 启动本地隧道

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
adg t stop TUNNEL_ID   # 用 adg t list 中的 ID 停止并移除隧道

adg daemon status
adg daemon start      # 可选；t 命令会自动启动
adg daemon stop       # 等待全部 tunnel 清理后退出

adg t team-a --no-daemon      # 在当前终端前台运行，Ctrl-C 退出
```

启动成功会输出 tunnel ID 和 `export ADB_SERVER_SOCKET=tcp:127.0.0.1:端口`。在使用 adb/Appium 的终端设置对应的环境变量即可选择 tunnel。重复提交相同参数会复用已有 tunnel；同一监听地址使用不同配置会报冲突，需要先停止旧 tunnel。

隧道先完成远端认证，再监听本地端口；只接受数值回环地址。本地端口是客户端的可信边界，同机能连接该端口的进程可使用它。服务端会根据证书身份和所选组再次授权每个数据通道，不使用端口号、来源 IP 或隧道 ID 代替身份。

## 连接参数

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

## 守护进程与断线重连

普通托管 tunnel 首次连接成功后，server 重启或网络断开会使其进入 `reconnecting`。daemon 保留原本地端口、分组和本地 tunnel ID，重新读取凭据并建立新的认证会话。不可用期间新 ADB 请求立即返回 `FAIL`，已有连接结束；恢复后后续命令通过原 socket 即可访问，无需再次 `use`。forward/reverse 规则需要重新创建，ADB 命令不会重放。

重连采用从 1 秒增长至 30 秒的指数退避，带 ±20% 抖动；持续在线 30 秒后重置，每轮连接限时 30 秒。网络故障、HTTP 5xx/429/408 和暂时缺失的分组（404）会继续重试。证书、信任校验、协议或明确身份拒绝会停止自动重试并显示 `failed`，本地端口仍保留至停止 tunnel；修复后重复 `use` 或 `t` 可显式重试。首次连接失败、前台 `t GROUP --no-daemon` 和 `t GROUP -w` 写租约隧道仍需显式重试。daemon 不持久化 tunnel 配置，daemon 自身重启后需重新提交。

daemon 支持 macOS/Linux，控制 socket 位于 `~/.config/adbgroup/daemon/control.sock`，目录权限为 `0700`，socket 权限为 `0600`；日志写入同目录的 `daemon.log`。本地控制协议版本不兼容时会报错，应先使用旧版本命令停止 daemon，再使用新版本启动。首版不安装系统服务或配置开机自启。

升级到 daemon 控制协议 v3 前，请用旧版本 `adg` 执行 `adg daemon stop`。升级后，用新版本重新提交 tunnel。

## 正向和反向端口转发

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

如需跨组独占设备并建立写隧道，见[独占租约](api.zh-CN.md#独占租约)。
