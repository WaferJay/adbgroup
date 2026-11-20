# 网关 API

[项目首页](../README.zh-CN.md) · [English](api.md)

## 独占租约

普通设备连接自动获取读锁，结束时释放。读锁表示并发调度资格，允许运行会修改设备的命令；多连接命令不保证整体原子性。写锁等待或生效期间，新普通操作默认等待最多 30 秒，最多排队 128 个请求；长时间 logcat 等现有流可能使写锁申请超时。

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

## REST 接口

以下接口全部要求 mTLS；路径参数按 URL 路径段编码。分组配置由 YAML 管理，API 不提供分组成员写接口。

| 方法与路径 | 含义 |
| --- | --- |
| `GET /v1/groups` | 列出授权组，静态组在前，自动组随后 |
| `GET /v1/groups/{group}` | 组名及有效设备成员列表 |
| `GET /v1/groups/{group}/devices` | 组设备状态及锁状态，包括断开的成员 |
| `GET /v1/groups/{group}/devices/{device}` | 单设备状态和 mTLS 隧道入口描述 |
| `POST /v1/groups/{group}/devices/{device}/write-leases` | 正文含可选 wait_seconds、ttl_seconds |
| `GET /v1/groups/{group}/devices/{device}/write-leases/{lease}` | 查询自己的租约 |
| `PATCH /v1/groups/{group}/devices/{device}/write-leases/{lease}` | 设置 expires_at |
| `DELETE /v1/groups/{group}/devices/{device}/write-leases/{lease}` | 幂等释放 |
| `POST /v1/groups/{group}/tunnels` | 正文 {} 或 {"lease_id":"..."}；创建 pending 隧道 |
| `GET /v1/groups/{group}/tunnels/{tunnel}` | 查询自己的 pending/connecting/active/closed 隧道 |
| `DELETE /v1/groups/{group}/tunnels/{tunnel}` | 幂等关闭，终态保留一小时 |
| `GET /v1/groups/{group}/tunnels/{tunnel}/control` | 升级为 WebSocket 控制连接 |
| `POST /v1/groups/{group}/tunnels/{tunnel}/streams` | 正文 {"kind":"adb"} 或 {"kind":"forward","rule_id":"..."} |
| `GET /v1/groups/{group}/tunnels/{tunnel}/streams/{stream}` | 升级为单次接入的二进制 WebSocket 数据流 |

隧道和数据流返回 `id`、`path`，创建成功同时返回 Location。ID 不代表授权；资源绑定身份、分组、隧道和用途。无权限或不存在的资源返回 404，未授权身份返回 403，状态冲突/流上限返回 409，隧道数量超限返回 429。正文最多 4 KiB，拒绝未知字段和多个 JSON 值。

## WebSocket 隧道协议

控制消息是版本 1 JSON：`version,id,op,rule,previous,stream,port,error`。服务端 ready 后，客户端处理 bind、activate、remove、remember、forget、reverse，以同一 id 回复 reply；转发绑定与激活分两阶段，监听失败不覆盖既有规则。reverse 数据流由服务端创建，通过 control 告知所属客户端，仅允许接入已确认规则的回环目标。

每条数据 WebSocket 使用二进制消息，无压缩；每个本地 ADB 或转发连接独占一条数据 WebSocket，控制消息独立传输。客户端 WebSocket 使用 HTTP/1.1，反向代理需支持 WebSocket Upgrade 和长连接，且必须保留端到端 mTLS 身份认证；不信任代理传入的身份 Header。

隧道/数据流须在创建后 10 秒内接入；控制消息上限 64 KiB，待处理请求上限 64，响应超时 10 秒，心跳每 15 秒一次、等待 10 秒。每隧道最多 128 条数据流，每身份最多 32 个未关闭隧道。长数据流无固定总时限，仍受证书、权限、设备代次和租约约束。
