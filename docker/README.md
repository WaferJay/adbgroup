# Docker 部署

此目录用于在 Linux Docker 主机上运行 adbgroup 服务端。`compose.yaml` 只引用
镜像，不依赖仓库源码，可以连同配置示例单独复制到部署主机。`compose.build.yaml`
只用于在本仓库内从源码构建镜像。

容器以固定的非 root 用户运行，不需要 `privileged: true`。macOS 和 Windows 上的
Docker 运行在虚拟机中，不能按此方式直接访问宿主机 USB 设备。

## 获取镜像

在本仓库内构建本地镜像：

```sh
cd docker
docker compose -f compose.yaml -f compose.build.yaml build
```

这会生成默认名称 `adbgroup:local`。也可以把镜像推送到私有或公共镜像仓库；独立
部署时通过 `ADBGROUP_IMAGE` 指定包含版本的完整镜像名：

```sh
export ADBGROUP_IMAGE=registry.example.com/adbgroup:1.0.0
docker compose pull
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

容器需要访问 `/dev/bus/usb`。先确认相应 USB 设备节点的组，并把数字 GID 传给
Compose：

```sh
ls -ln /dev/bus/usb/*/*
export USB_GID="$(getent group plugdev | cut -d: -f3)"
```

如果发行版没有 `plugdev`，请使用 udev 规则为 Android USB 设备设置一个专用组，
然后将该组的数字 GID 赋给 `USB_GID`。Compose 默认值 `46` 只适用于使用该 GID
作为 `plugdev` 的系统。udev 规则必须在宿主机配置；容器内不运行 udev。

rootless Docker 通常不能直接授予 USB 字符设备访问权。此部署方式应使用能够配置
设备 cgroup 规则的 Docker Engine。

## 启动与检查

```sh
docker compose run --rm adbgroup check -config /etc/adbgroup/config.yaml
docker compose up -d
docker compose logs -f adbgroup
```

API 默认发布到宿主机的 `8443` 端口。可以在启动前通过环境变量修改宿主机端口：

```sh
ADBGROUP_PORT=9443 docker compose up -d
```

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
docker compose up -d --force-recreate
```

停止服务但保留 `data/`：

```sh
docker compose down
```
