# Docker 部署

此目录用于在 Linux Docker 主机上构建和运行 adbgroup 服务端。`compose.yaml` 同时
声明镜像名称和源码构建方式：在仓库内可以构建镜像；独立部署时可以连同配置示例
复制到部署主机，并使用已有或从镜像仓库拉取的镜像。

容器以固定的非 root 用户运行，不需要 `privileged: true`。macOS 和 Windows 上的
Docker 运行在虚拟机中，不能按此方式直接访问宿主机 USB 设备。

## 获取镜像

GitHub Actions 会在 `main` 更新、推送 `v*` 标签或手动触发时构建并发布
`linux/amd64` 和 `linux/arm64` 镜像。面向 `main` 的 PR 只验证构建，不发布。
`latest` 和 `main` 跟随主分支，也可以使用 `v1.0.0` 等版本标签或
`sha-<短提交号>` 标签固定部署版本。

首次发布后，如需匿名拉取，请将 GHCR 包的可见性设为 public。
使用 CI 发布的镜像：

```sh
cd docker
export ADBGROUP_IMAGE=ghcr.io/waferjay/adbgroup:latest
docker compose pull
```

将 `ADBGROUP_IMAGE=ghcr.io/waferjay/adbgroup:latest` 写入本目录 `.env`，后续
Compose 命令即可复用。生产部署可将 `latest` 替换为已发布的具体版本标签。

在本仓库内构建本地镜像：

```sh
cd docker
docker compose build
```

这会生成默认名称 `adbgroup:local`。也可以把镜像推送到私有或公共镜像仓库；独立
部署时通过 `ADBGROUP_IMAGE` 指定包含版本的完整镜像名：

```sh
export ADBGROUP_IMAGE=registry.example.com/adbgroup:1.0.0
docker compose pull
```

独立部署目录不需要仓库源码。运行时使用 `--no-build`，明确禁止 Compose 尝试源码
构建：

```sh
docker compose up -d --no-build
```

如果不使用镜像仓库，可以在构建机器上执行 `docker save adbgroup:local`，传输归档
后在部署主机执行 `docker load`。此时无需设置 `ADBGROUP_IMAGE`。

## 准备配置

```sh
cd docker
cp config.example.yaml config.yaml
sudo install -d -o 10001 -g 10001 -m 0700 data
```

启动前编辑 `config.yaml`：

- 把设备序列号写入 `groups[].devices`；
- 配置 `identities`；
- 在第一次启动前，把外部访问使用的域名或 IP 写入
  `pki.server_names`。证书生成后，修改该项不会自动重签证书。

`config.yaml`、`data/` 和 `clients/` 已被本目录的 `.gitignore` 忽略，不应提交
证书身份配置、PKI、客户端私钥或其他部署信息。

容器使用 UID/GID `10001:10001`，所以宿主机的 `data/` 必须归该用户所有且可写。
不要为了省略所有权设置而把目录权限改为 `0777`。

## USB 权限

容器需要同时具备 USB 设备的 cgroup 访问权和设备节点的文件读写权限。Compose
已通过整个 `/dev/bus/usb` 目录的 bind mount 和 `c 189:* rmw` 规则支持热插拔；
非 root 进程能否打开新设备，还取决于宿主机 udev 为新节点设置的组和权限。

在 **Linux Docker 宿主机** 上、此目录内执行：

```sh
getent group plugdev || sudo groupadd --system plugdev
sudo install -o root -g root -m 0644 70-android-adb.rules \
  /etc/udev/rules.d/70-android-adb.rules
sudo udevadm control --reload-rules
export USB_GID="$(getent group plugdev | cut -d: -f3)"
```

`70-android-adb.rules` 是通用的 Linux ADB USB 权限规则，可用于宿主机直接运行
ADB 或容器部署。规则通过 ADB USB 接口特征 `ff4201` 匹配设备，为设备节点设置 `plugdev` 组和
`0660` 权限，也支持包含 ADB 接口的复合 USB 设备及后续接入的新厂商设备。手机
需要开启 USB 调试；规则不匹配没有 ADB 接口的普通 USB 设备或 fastboot 模式。
独立部署时，也应将本目录的规则文件一起复制到宿主机。

把上面得到的数字 GID 写入本目录 `.env` 的 `USB_GID=数字`，后续 Compose 命令
即可复用；若使用已有 USB 专用组，应同时修改规则的 `GROUP` 和 `USB_GID`。
Compose 默认值 `46` 只适用于宿主机 `plugdev` 确实使用该 GID 的情况。容器与
宿主机按数字 GID 判断权限，组名可以不同。仅在宿主机给登录用户加入 `plugdev`
不会改变容器进程的组。

已有容器更改 `USB_GID` 后需要重建，`docker compose restart` 不会更新附加组：

```sh
docker compose up -d --no-build --force-recreate
```

重建容器会中断当前连接。若容器已经包含正确的数字 GID，只需安装和应用 udev
规则，无需重建容器。

重新插拔报错的那台设备，使 udev 为新节点应用规则。只执行 `--reload-rules`
不会修改已经存在的设备节点；如需保留连接，可以针对该设备触发规则，下面路径
需要替换为它当前的 USB 节点：

```sh
USB_NODE=/dev/bus/usb/001/005
sudo udevadm trigger --action=change \
  "/sys$(udevadm info --query=path --name="$USB_NODE")"
sudo udevadm settle
```

若触发规则后 ADB 仍保留 `no permissions` 状态，只重新插拔该设备，让 ADB
重新枚举。

检查容器的实际身份、节点权限和专用 ADB server 的设备状态：

```sh
docker compose exec adbgroup id
docker compose exec adbgroup sh -c 'ls -ln /dev/bus/usb/*/*'
docker compose exec adbgroup adb -P 5038 devices -l
```

目标节点应有组读写权限（如 `crw-rw----`），其数字 GID 应出现在容器 `id`
输出的组列表中。`no permissions (missing udev rules? user is in the plugdev
group)` 仍然表示 ADB 无法打开 USB 节点，不能据此认定节点权限正确。若状态变为
`unauthorized`，则在手机上确认 USB 调试授权；这与文件权限是不同的步骤。

手动 `chmod` 只对当前节点有效，重新插拔后会失效。udev 规则必须在宿主机配置；
容器内不运行 udev。修复不需要重建镜像；规则和容器附加组配置完成后，后续新增
设备或再次插拔设备无需重建容器或重启 ADB server。

参考：[Android Linux 设备权限说明](https://developer.android.com/studio/run/device)、
[Docker 附加组说明](https://docs.docker.com/engine/containers/run/#additional-groups)。

rootless Docker 通常不能直接授予 USB 字符设备访问权。此部署方式应使用能够配置
设备 cgroup 规则的 Docker Engine。

## 启动与检查

```sh
docker compose run --rm adbgroup check -config /etc/adbgroup/config.yaml
docker compose up -d --no-build
docker compose logs -f adbgroup
```

API 默认发布到宿主机的 `8443` 端口。可以在启动前通过环境变量修改宿主机端口：

```sh
ADBGROUP_PORT=9443 docker compose up -d --no-build
```

容器内的原生 ADB server 默认通过宿主机回环地址 `127.0.0.1:5039` 访问：

```sh
adb -H 127.0.0.1 -P 5039 devices
```

如需修改宿主机端口：

```sh
ADBGROUP_ADB_PORT=15038 docker compose up -d --no-build
adb -H 127.0.0.1 -P 15038 devices
```

Docker 配置中的裸 `backend.address: 5038` 会让 ADB server 监听容器所有接口，供
端口发布使用；adbgroup 自身仍通过容器回环地址连接它。Compose 将原始 ADB 端口
限制在宿主机 `127.0.0.1`，因为该端口不经过 mTLS、分组授权或设备锁，不应改为
公网监听。

宿主机的 `data/` 目录保存自动生成的 CA、服务端证书以及 ADB 主机密钥。升级或
重建容器不会更换这些凭据。请把该目录作为敏感数据备份；删除它会丢失 CA 私钥，
已签发的客户端证书也将无法继续使用。

## 签发客户端证书

服务启动并生成 PKI 后，可以在运行中的容器里签发客户端证书。输出目录必须是
`data/` 内尚不存在的目录：

```sh
docker compose exec adbgroup adbgroup cert \
  -config /etc/adbgroup/config.yaml \
  -user alice \
  -out /var/lib/adbgroup/clients/alice
```

这些文件位于宿主机的 `data/clients/alice/`。私钥权限是 `0600`，且所有者为数字
UID `10001`；如需交给客户端，可以保持原权限复制出来：

```sh
docker compose cp \
  adbgroup:/var/lib/adbgroup/clients/alice ./clients/alice
```

私钥复制完成后应妥善保管，并按需删除 `data/` 中的客户端导出目录。

## 重载与停止

修改可热更新的分组成员或身份授权后：

```sh
docker compose kill -s HUP adbgroup
```

后端、监听、PKI 或分组名称变化需要重建容器：

```sh
docker compose up -d --no-build --force-recreate
```

停止服务但保留 `data/`：

```sh
docker compose down
```
