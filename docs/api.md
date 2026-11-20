# Gateway API

[Project overview](../README.md) · [简体中文](api.zh-CN.md)

## Exclusive leases

Standard device connections acquire a read lock automatically and release it when they end. A read lock grants concurrent scheduling access and allows commands that modify the device; commands using multiple connections are not guaranteed to be atomic as a whole. While a write lock is pending or active, new standard operations wait up to 30 seconds by default, with a queue of at most 128 requests. Existing long-lived streams such as logcat can cause a write-lock request to time out.

Use the same certificate to request a global write lock on a physical device:

```sh
curl --cacert run/clients/alice/ca.pem \
  --cert run/clients/alice/client.pem --key run/clients/alice/client.key \
  -H 'Content-Type: application/json' \
  -d '{"wait_seconds":30,"ttl_seconds":300}' \
  https://adb-host.example:8443/v1/groups/team-a/devices/USB_SERIAL_A/write-leases
```

The response includes status `201`, a `Location` header pointing to the canonical nested resource, and `{"lease": ...}`. It contains no password or SSH credentials. Poll the Location until the lease is active, then establish an explicit write tunnel:

```sh
adg t -s https://adb-host.example:8443 -d run/clients/alice \
  team-a -w LEASE_ID -l 15039
adb -H 127.0.0.1 -P 15039 -s USB_SERIAL_A shell
```

A write tunnel requires the same certificate identity, the same group, and a valid active lease. It can access only the device bound to that lease. A standard tunnel does not upgrade automatically. Users can reconnect to an unexpired lease with a new certificate for the same identity. Tunnel disconnection does not release the lease automatically; its lifetime is controlled by TTL or DELETE.

Write locks wait in FIFO order for existing read connections to drain, without preempting active streams. The wait defaults to 30 seconds and accepts 1–3600 seconds. The lease TTL starts when the lease becomes active, defaults to 300 seconds, and accepts 1–1800 seconds. `PATCH {"expires_at":"RFC3339"}` sets an exact expiration time, which must be in the future and no more than 30 minutes from the current time. Only active, unexpired leases can be updated. DELETE cancels a queued request or releases a lease and is idempotent.

Unplugging a device invalidates its sessions and leases and clears its forward/reverse rules and local listeners. Other devices' rules and connections keep working. Reconnecting restores group membership by serial number, but forwarding rules must be recreated by the user or tooling. A new forward waits for the old listener's cleanup and creates a listener independent of the previous device generation. Old transport IDs, leases, and rules cannot be reused. Access is denied for duplicate serial numbers.

## REST endpoints

All endpoints require mTLS. Encode path parameters as URL path segments. Group configuration is managed in YAML; the API does not provide endpoints to modify group membership.

| Method and path | Description |
| --- | --- |
| `GET /v1/groups` | List authorized groups, static groups before automatic groups |
| `GET /v1/groups/{group}` | Group name and effective device membership |
| `GET /v1/groups/{group}/devices` | Group device and lock states, including disconnected members |
| `GET /v1/groups/{group}/devices/{device}` | Device state and mTLS tunnel endpoint description |
| `POST /v1/groups/{group}/devices/{device}/write-leases` | Body accepts optional wait_seconds and ttl_seconds |
| `GET /v1/groups/{group}/devices/{device}/write-leases/{lease}` | Query your own lease |
| `PATCH /v1/groups/{group}/devices/{device}/write-leases/{lease}` | Set expires_at |
| `DELETE /v1/groups/{group}/devices/{device}/write-leases/{lease}` | Release idempotently |
| `POST /v1/groups/{group}/tunnels` | Body {} or {"lease_id":"..."}; create a pending tunnel |
| `GET /v1/groups/{group}/tunnels/{tunnel}` | Query your own pending/connecting/active/closed tunnel |
| `DELETE /v1/groups/{group}/tunnels/{tunnel}` | Close idempotently; retain terminal state for one hour |
| `GET /v1/groups/{group}/tunnels/{tunnel}/control` | Upgrade to a WebSocket control connection |
| `POST /v1/groups/{group}/tunnels/{tunnel}/streams` | Body {"kind":"adb"} or {"kind":"forward","rule_id":"..."} |
| `GET /v1/groups/{group}/tunnels/{tunnel}/streams/{stream}` | Upgrade to a binary WebSocket data stream that accepts a single connection |

Tunnels and data streams return `id` and `path`, plus a Location header on successful creation. An ID does not grant authorization: resources are bound to an identity, group, tunnel, and purpose. Missing resources or resources outside the caller's permissions return 404; unauthorized identities return 403; state conflicts or stream limits return 409; tunnel count limits return 429. Request bodies are limited to 4 KiB, and unknown fields or multiple JSON values are rejected.

## WebSocket tunnel protocol

Control messages use version 1 JSON: `version,id,op,rule,previous,stream,port,error`. After the server sends ready, the client handles bind, activate, remove, remember, forget, and reverse, replying with reply and the same id. Forward binding and activation are separate phases, so a failed listener does not overwrite an existing rule. The server creates reverse data streams and notifies the owning client through control; connections can reach only the loopback target of an acknowledged rule.

Each data WebSocket uses binary messages without compression. Each local ADB or forwarding connection has a dedicated data WebSocket, while control messages travel separately. Client WebSockets use HTTP/1.1. Reverse proxies must support WebSocket Upgrade and long-lived connections while preserving end-to-end mTLS authentication. Identity headers supplied by a proxy are not trusted.

Tunnels and data streams must connect within 10 seconds of creation. Control messages are limited to 64 KiB, with at most 64 pending requests and a 10-second response timeout. Heartbeats are sent every 15 seconds with a 10-second wait. Each tunnel supports at most 128 data streams; each identity supports at most 32 tunnels that have not been closed. Long-lived data streams have no fixed total duration but remain subject to certificate validity, authorization, device generation, and lease constraints.
