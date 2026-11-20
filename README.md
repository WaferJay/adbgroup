# ADB Group Gateway

**English** | [简体中文](README.zh-CN.md)

Group Linux USB devices with unified mTLS authentication, native ADB access, and global read/write locks on physical devices. Each user has a client certificate shared by the API and tunnels, with a single authorization configuration. A team is a virtual group; it requires no Linux account, system sshd, or SSH public key.

```text
Native adb / Appium → local adbgroup tunnel → mTLS HTTPS → groups and locks → persistent ADB server → USB
```

## Build and start the server

Requires Go 1.26+, recent Android Platform Tools, and Linux USB/udev permissions.

```sh
go build -o bin/adg ./cmd/adbgroup
cp config.example.yaml config.yaml
bin/adg check -config config.yaml
bin/adg serve -config config.yaml
```

`adg --help` (or `-h`, `-help`) lists commands. In an interactive terminal with
shell integration enabled, `adg` and `adg use` both open the group selection menu.
Use `adg <command> -help` to see flags and defaults, for example `adg t -help`,
`adg t stop -help`, or `adg daemon -help`. Help commands exit successfully without
reading configuration or starting services.

The REST API and WebSocket tunnels share one HTTPS endpoint. When `backend.address` is a
numeric loopback address such as `127.0.0.1:5038`, the ADB server listens only on loopback.
A bare port such as `5038` makes it listen on all interfaces for container port forwarding;
the gateway still connects internally through `127.0.0.1:5038`. The raw ADB port bypasses
mTLS, group authorization, and device locks. Map it explicitly to the host's loopback
address only; do not expose it to remote users.

`backend.manage: true` starts a persistent ADB server on a dedicated port and refuses to start if another process already uses that port. Unplugging or reconnecting one device does not restart the backend, so other devices keep running. An ADB server failure still interrupts all device connections.

USB enumeration supports both the native backend's `usb:` paths and the bus/port paths without that prefix used by some libusb versions. Network devices and emulators remain excluded.

## Docker images

The [Docker image workflow](.github/workflows/docker.yml) builds Linux images for
`amd64` and `arm64`. Pull requests targeting `main` verify the build without
publishing. Pushes to `main`, `v*` tags, and manual workflow runs publish to
`ghcr.io/waferjay/adbgroup` using the built-in `GITHUB_TOKEN`; no extra registry
secrets are required.

Before building images, the workflow runs `go vet ./...` and
`go test -race -count=1 -timeout=5m ./...` with the latest Go 1.26 patch release.
It installs ADB for native client compatibility tests, plus bash, zsh, and
`script` (from util-linux) for shell integration tests. Failed checks block image
builds and publication.

The `latest` and `main` tags track `main`. Git tags such as `v1.0.0` produce the
matching image tag, and each published build also gets a `sha-<short-commit>` tag.

```sh
docker pull ghcr.io/waferjay/adbgroup:latest
```

See [Docker deployment](docker/README.md) for configuration, USB permissions, and
Compose commands. For anonymous pulls, set the GHCR package visibility to public
after its first publication.

## Binary releases

Pushing a semantic version tag such as `v0.1.0` or `v0.1.0-beta.1` runs the
[CI and release workflow](.github/workflows/docker.yml). After tests and Docker
image publication succeed, the [release job](.github/workflows/release.yml) uses
[GoReleaser](.goreleaser.yaml) to publish a GitHub Release containing:

- Linux and macOS binaries for `amd64` and `arm64`, packaged as
  `adbgroup_<version>_<os>_<arch>.tar.gz`;
- `checksums.txt` with SHA-256 hashes;
- release notes generated from commits since the previous tag.

Each archive includes both READMEs, example server/client configuration, the
systemd service, and Linux USB rules. The Linux binary supports the server and
client; the macOS binary is for the client. Tags with a prerelease suffix are
marked as prereleases. Publishing uses the built-in `GITHUB_TOKEN`.

Once the release changes are committed and pushed, publish a version with:

```sh
git tag -a v0.1.0-beta.1 -m "Release v0.1.0-beta.1"
git push origin v0.1.0-beta.1
```

To check packaging locally with GoReleaser v2.20.0 without publishing:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Artifacts are written to `dist/`. Download a matching archive from the
[Releases page](https://github.com/WaferJay/adbgroup/releases), verify its checksum,
and place the extracted `adg` binary on your PATH.

The executable is named `adg`; the project, archive names, configuration paths,
service account, and certificate identities retain the name `adbgroup`. When
upgrading from an `adbgroup` executable, update scripts and shell initialization
to call `adg`.

## Automatic groups and wildcard authorization

The server can group devices by serial or ADB `model`. Each omitted block is disabled by default:

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

`configured` collects devices from static `groups`. `discovery` remembers devices from the existing USB monitor and exposes those outside the static configuration. `all` contains their union. Summary names default to `configured`, `discovery`, and `all`. Enabling `all` alone does not enable discovery or require the configured summary. Disconnected discovered devices stay in memory until process exit and are rediscovered after restart; no state file is used. Network devices and emulators are excluded from discovery.

Serial and model grouping require a `source` naming an enabled summary or a static group; generated groups cannot be sources. Serial groups use the original serial as their name, allowing `adg use USB_SERIAL_A`. Model groups use the raw ADB `model` value with a default `model-` prefix, without executing device shell commands. Unknown models are omitted until available; the last known model is kept in memory and replaced by new nonempty values.

`inherit_access` defaults to false. When enabled, a direct grant to a static, summary, or model group also grants the corresponding serial groups. Serial grants do not grant access to other groups. In this example Bob can select serial groups for devices in `model-Pixel_8`. Summary grants include future members. Identities may preauthorize future serial or model names when the corresponding rule is enabled.

A complete `"*"` entry in identities grants every existing and future group, including static and automatic groups. Partial patterns such as `model-*` are unsupported. Wildcards do not bypass group existence, device availability, active write leases, or physical device locks. Quote `"*"` in YAML. If several groups are authorized, you can target one directly with `adg GROUP <adb arguments>` or select one for the current terminal with `adg GROUP` after shell initialization.

All groups share one namespace. Conflicts known from configuration reject startup or reload. Runtime conflicts are logged and conflicting generated groups are withheld; static and summary groups remain intact. Changes to `auto_groups` require restart. Membership, identity, or model changes recompute groups and authorization, closing invalid accesses and revoking affected leases.

Group lists contain static groups in configuration order, then the configured, discovery, and all summaries, followed by serial and model groups sorted by name within each type. Automatic members are sorted and deduplicated by serial, including disconnected devices.

## Certificates

If `api.certificate`, `api.key`, and `api.client_ca` are all omitted, the first startup generates a CA and server credentials in `pki.directory` (default: `run/pki` under the configuration directory). Later startups reuse them. Specify either all three settings or none.

```yaml
api:
  listen: 0.0.0.0:8443
pki:
  directory: run/pki
  server_names: [adb-host.example, 192.168.1.10]
```

The generated files are `ca.pem`, `ca.key`, `server.pem`, and `server.key`. Directory permissions are `0700`; file permissions are `0600`. The CA is valid for 10 years by default. The server certificate is valid for 2 years by default and never outlives the CA. SANs include localhost, loopback IPs, the hostname, and current local interface IPs. Add public or NAT-facing domain names and IPs through `server_names` before the first initialization.

Repeated startups, network changes, or additional SANs do not overwrite certificates automatically. Incomplete, damaged, or expired credentials produce an error and require an administrator to restore or renew them. Replacing the server certificate or CA trust file requires a full restart.

With managed PKI, each `adg serve` startup provisions client PEM files and a `client.adgpack` for new identities in `identities`. For `spiffe://adbgroup/alice`, the default pack path is `run/pki/clients/alice/client.adgpack`. Transfer it privately to Alice, who can run `adg profile import client.adgpack`. Other identity URIs have separate export directories. The issuance log records each pack path.

On startup, the server rebuilds missing packs and atomically replaces packs whose advertised HTTPS addresses have changed, using the original PEM files. After changing the `api.listen` port, restart the server and distribute the refreshed pack. Clients with a profile for the old address can import the pack under a new name with `adg profile import -name alice-new client.adgpack`.

SIGHUP reloads authorization. Added identities receive their packs on the next startup. With external PKI, administrators issue client credentials with their signing CA.

Administrators can also issue user credentials explicitly:

```sh
bin/adg cert -config config.yaml -user alice -out run/clients/alice
# Custom identity and validity period
bin/adg cert -config config.yaml \
  -identity spiffe://adbgroup/ci-runner -days 90 -out run/clients/ci-runner
```

The output directory contains `client.adgpack` plus the original `client.pem`, `client.key`, and `ca.pem`; the CA private key is never exported. An existing output directory is rejected. `-out` is relative to the command's working directory, while PKI paths are relative to the configuration file's directory. The CLI can initialize the default PKI before the first server startup without starting listeners or ADB.

`-user alice` maps to `spiffe://adbgroup/alice`. Use the exact identity URI from the client certificate as the key in `identities`. The server rejects identities using the `urn:` scheme during configuration checks, startup, and reload. Issuing a certificate does not grant access automatically. Add the identity to the configuration, then send SIGHUP:

```yaml
identities:
  "spiffe://adbgroup/alice": [team-a]
```

New user certificates issued by the same CA do not require a restart. The server requires exactly one URI SAN in the certificate, and that URI must appear in the authorization configuration. Validity can be 1–3650 days, defaults to 365 days, and cannot exceed the issuing CA's remaining lifetime.

For an enterprise PKI, explicitly configure all three API credential files. Use `-ca-key` and optionally `-ca-cert` to select a trusted issuer when issuing client certificates. The exported `ca.pem` contains the client certificate's issuing CA chain. If a different CA signs the HTTPS server certificate, the tunnel's `-ca` and curl's `--cacert` must use the server CA trust file.

## Standard clients and transparent forwarding

Install the same binary on the client. Transfer `client.adgpack` over a private channel such as SCP. The pack contains the user's unencrypted private key. The suggested profile name is the original export directory name.

```sh
adg profile import client.adgpack
adg groups
adg profile import -name prod prod.adgpack
adg profile list
adg profile set-default prod
adg use -p alice team-a             # select in this terminal (with shell integration)
adg -p prod team-a devices -l       # one invocation
adg profile update alice renewed.adgpack
```

Issuance uses configured `pki.server_names` covered by the actual HTTPS certificate and the port from `api.listen`. Without configured names it uses concrete certificate SANs. One candidate is automatic. For multiple candidates the importer offers a menu. For scripts, missing candidates or port mappings, pass `-server https://host:port` when issuing or importing. For a separate HTTPS CA, pass `-server-ca gateway-ca.pem` when issuing or repacking.

```sh
adg cert pack -config config.yaml -credentials run/clients/alice -bundle alice.adgpack
adg profile import -f staging.yaml -name staging
```

The first profile becomes the default on a fresh installation. Later imports coexist. Renewals keep existing tunnels and their reconnects on their original credential version. A changed gateway or identity needs a separate name. Updating server CA trust requires interactive confirmation of the displayed SHA-256 fingerprints. The local store under `~/.config/adbgroup/profiles/` retains earlier credential versions for existing sessions.

Existing clients continue to use `~/.config/adbgroup/tunnel.yaml` until a profile is selected as default. Explicit `-f` selects a YAML configuration and is mutually exclusive with `-p`. Example legacy client YAML:

```yaml
server: https://adb-host.example:8443
# Optional; defaults to the directory containing tunnel.yaml
credentials: /path/to/run/clients/alice
```

When upgrading, remove the top-level `group:` field from existing client YAML files. Pass the group on the command line or select one with `adg use GROUP` for device queries.

The `credentials` directory uses the fixed filenames produced by the `cert` command: `client.pem`, `client.key`, and `ca.pem`. You can also place these files next to `tunnel.yaml` and omit `credentials`.

### Run adb for a group

With Android Platform Tools' `adb` on your PATH, put the group name before the adb arguments. Each invocation selects its own group and works from a fresh terminal, a script, or CI without shell initialization or exports.

```sh
adg team-a devices -l                     # List devices in team-a
adg team-b -s USB_SERIAL_B shell          # Open a shell on one device in team-b
adg -f staging.yaml team-a devices        # Use a different client config
adg @devices devices -l                   # The group itself is named "devices"
```

Put client connection options before the group. For example, `-s` before the group selects the gateway server, and `-s` after the group selects an adb serial. Built-in commands and aliases take precedence in the first position. Prefix a group name with `@` to select it explicitly. Scripts can use `adg "@$group" devices -l` for any group name, including a command or alias. To address a group named `@lab`, write `@@lab`. Existing scripts can continue to use `-g GROUP` or `-group GROUP`.

The daemon reuses a normal tunnel for the server, credentials, and group, or opens one on an available loopback port. A one-off command uses its own client configuration and group, even inside a shell with another gateway or group selected. The native adb process gets the original argument boundaries, standard input, output, and error streams. Its exit status is the command's exit status. The tunnel supplies the ADB server endpoint, so native adb server options `-H`, `-P`, and `-L` conflict with group selection. For a write lease, use `adg t GROUP -w <lease-id>`.

### Start a local tunnel

To start a tunnel for adb or another ADB client such as Appium, run:

```sh
adg t team-a
```

By default, `t` automatically starts a background daemon for the current user, hands the tunnel to it, and returns once the local listener is ready. The tunnel keeps running after the terminal closes. One daemon can manage tunnels to multiple servers or groups, with a different local listening port for each tunnel:

```sh
adg t team-a -l 15037
adg t team-b -l 15038
adg t @list -l 15040   # Start a tunnel for the group named "list"
adg t list             # ID, server, group, listener, status, latest error
adg t list --json
adg t stop <id>        # Stop and remove a tunnel

adg daemon status
adg daemon start      # Optional; t starts it automatically
adg daemon stop       # Exit after all tunnels finish cleanup

adg t team-a --no-daemon      # Run in the foreground; exit with Ctrl-C
```

A successful startup prints the tunnel ID and `export ADB_SERVER_SOCKET=tcp:127.0.0.1:<port>`. Set that environment variable in the terminal running adb or Appium to select the tunnel. Submitting the same parameters again reuses an existing tunnel. Using different settings for the same listening address causes a conflict; stop the old tunnel first.

### Select a group for the current terminal

With shell integration, `adg GROUP` is shorthand for `adg use GROUP`. Subsequent `adb` commands in that terminal use the selected group. To enable this in zsh, add the following line to `~/.zshrc` and run it in your current terminal.

```sh
eval "$(command adg shell-init zsh)"
```

For bash, use `eval "$(command adg shell-init bash)"` in the startup file your terminal loads (usually `~/.bashrc`; login shells should source it from `~/.bash_profile`). Initialization is safe to repeat and does not edit these files automatically.

`adg devices` queries the group selected in this terminal. In a fresh terminal, or when supplying connection options such as `-f`, run `adg devices GROUP`. Creating a tunnel always requires an explicit group.

```sh
adg use team-a         # Select team-a for this terminal
adg devices --json     # Print its gateway inventory
adb devices -l         # List devices in team-a
adg team-b devices -l  # List devices in team-b once
echo "$ADBGROUP_GROUP" # Still team-a
adg @devices           # Select a group named "devices" (also: adg use @devices)
adg                    # Open the same menu as adg use. Press Enter or q to cancel.
adg d                  # Query the currently selected group
adg g                  # A leading * marks the group selected in this terminal
adg unuse              # Restore ADB_SERVER_SOCKET from before the first use

# Explicit connection settings are retained by subsequent use/g/d calls
adg use team-a -s https://adb-host.example:8443 -d run/clients/alice
```

Device queries and tunnel creation preserve the current terminal selection.

`use` matches normal tunnels by server, credential paths, and group, including tunnels created with `t`. If none matches, the OS assigns a loopback port; the configured `listen` is not used. Matching tunnels are preferred in active, starting, reconnecting, then failed order, with ties ordered by tunnel ID. For reconnecting tunnels, `use` waits for recovery without creating another listener; cancelling that wait leaves reconnection running. Failed tunnels are retried explicitly. A failed or cancelled switch preserves the previous selection. `use` does not select write leases; continue using `t GROUP -w` for write access.

Selection lives in the current shell environment and does not modify `tunnel.yaml`. adb/Appium processes subsequently launched from that shell inherit the socket; existing processes keep their environment. Explicit configuration, server, or credential options use the original client configuration rules instead of the selection. If you change `ADB_SERVER_SOCKET` manually, a selection context that no longer matches is ignored.

`ADBGROUP_GROUP` contains the name of the group currently selected in this shell, for prompts and scripts. A failed or cancelled switch preserves the previous value. `adg unuse` unsets it. After upgrading, run the shell initialization line again in existing terminals to enable the shorthand.

Without shell integration, switching the terminal group and `adg unuse` show setup instructions. To run adb once, append its arguments to the group, as in `adg team-a devices -l`.

When upgrading to daemon control protocol v3, run `adg daemon stop` with the old binary before upgrading, then submit your tunnels again with the new binary.

After a managed normal tunnel has connected successfully, server restarts and network outages change its state to `reconnecting`. The daemon retains the local port, group, and local tunnel ID, reloads credentials, and establishes a fresh authenticated remote session. New ADB requests receive `FAIL` while unavailable; existing connections end. Once connected again, subsequent ADB commands work through the same socket without another `use`. Forward/reverse rules must be recreated, and commands are never replayed.

Retries use exponential backoff from 1 to 30 seconds with ±20% jitter; 30 seconds continuously online resets the backoff. Each connection attempt is limited to 30 seconds. Network failures, HTTP 5xx/429/408, and temporarily missing groups (404) are retried. Certificate, trust, protocol, or explicit authorization failures stop automatic retries and show `failed`; the local port remains reserved until the tunnel is stopped. Fix the cause and repeat `use` or `t` to retry. Initial setup failures, foreground `t GROUP --no-daemon`, and write tunnels using `t GROUP -w` require explicit retry. The daemon does not persist tunnel configurations; submit them again after the daemon itself restarts.

The daemon supports macOS and Linux. Its control socket is `~/.config/adbgroup/daemon/control.sock`, with directory permissions `0700` and socket permissions `0600`. Logs go to `daemon.log` in the same directory. An incompatible local control protocol version produces an error; stop the daemon with the old binary before starting it with the new one. The initial release does not install a system service or configure startup at boot.

Without a configuration file, a remote connection can also be started with:

```sh
adg t -s https://adb-host.example:8443 -d run/clients/alice team-a

# In another terminal
export ADB_SERVER_SOCKET=tcp:127.0.0.1:15037
adb devices -l
adb -s USB_SERIAL_A shell
adb -s USB_SERIAL_A push app.apk /data/local/tmp/
adb -s USB_SERIAL_A install app.apk
```

`t` is an alias for `tunnel`. The short flags `-s/-l/-d/-w/-f` correspond to `-server/-listen/-credentials/-lease/-config`. Command-line flags override the configuration file. Without a default configuration file, the built-in server is `https://127.0.0.1:8443`, the credentials directory is the current directory, and the local listener is `127.0.0.1:15037`. The individual `-cert`, `-key`, and `-ca` flags override credential directory defaults.

`-l 15038` is equivalent to `-l 127.0.0.1:15038`. The configuration file also accepts `listen: 15038`. Full numeric IPv4/IPv6 loopback addresses remain supported.

`groups/g` and `devices/d` query the gateway's REST API directly with the client configuration and mTLS credentials.

```sh
adg g                  # List authorized groups (NAME) and their device counts (DEVICES)
adg d                  # Devices, connection states, and lock states in the selected group
adg d team-b           # Devices in a specific group, including offline devices
adg g --json            # Group list as JSON
adg d @devices --json  # Inventory for the group named "devices" as JSON
```

Query commands accept `-s/-d/-f`, their long forms, and `-cert/-key/-ca`. Command-line flags override the configuration file. Failed queries return a nonzero exit code, errors go to stderr, and JSON results go separately to stdout.

The tunnel authenticates with the remote server before opening the local listener and accepts only numeric loopback addresses. The local port is the client's trust boundary: any process on the same machine that can connect to it can use it. The server reauthorizes every data channel against the certificate identity and selected group. Port numbers, source IPs, and tunnel IDs do not replace identity.

Standard device connections acquire a read lock automatically and release it when they end. A read lock grants concurrent scheduling access and allows commands that modify the device; commands using multiple connections are not guaranteed to be atomic as a whole. While a write lock is pending or active, new standard operations wait up to 30 seconds by default, with a queue of at most 128 requests. Existing long-lived streams such as logcat can cause a write-lock request to time out.

Native `adb -s USB_SERIAL_A wait-for-device` is supported. It waits while the device is disconnected, offline, or unauthorized, then returns when the device enters the `device` state. Other supported commands are `wait-for-recovery`, `wait-for-rescue`, `wait-for-sideload`, `wait-for-bootloader`, and `wait-for-disconnect`. Waiting observes only devices in the current group, takes no read lock, and is not subject to the read-lock wait timeout. With multiple devices, select a target using `-s` or `-t`. For USB devices, `wait-for-disconnect` completes when the device disappears from the list; going offline alone does not count as a disconnect. Ctrl-C, tunnel closure, or authorization revocation ends the wait.

The host side of both forward and reverse runs on the user's computer:

```sh
adb -s USB_SERIAL_A forward tcp:8080 tcp:80
# Connect to 127.0.0.1:8080 on your computer to reach TCP port 80 on the device
adb -s USB_SERIAL_A forward tcp:0 localabstract:example
adb forward --list
adb -s USB_SERIAL_A forward --remove tcp:8080

# An app runs on your computer at port 18080; the device reaches it on its own port 8080
adb -s USB_SERIAL_A reverse tcp:8080 tcp:18080
adb -s USB_SERIAL_A reverse --list
adb -s USB_SERIAL_A reverse --remove-all
```

Forward rules are isolated per tunnel. ADB success is returned only after the client listener binds successfully; `tcp:0` uses a port assigned by the client's OS. Actual binding conflicts on the same computer produce an error. Rebinding for the same device is allowed by default; `--no-rebind` refuses to overwrite a rule. Device targets support TCP, localabstract, and JDWP.

Reverse supports TCP listeners on the device and TCP loopback targets on the client. Device ports are shared by the physical device: other groups authorized for that device can list, remove, or rebind rules. The data target always belongs to the tunnel that created the rule. Cleanup of an old tunnel or lease does not remove a newer rule that another tunnel has installed in its place.

The gateway and local ADB server connect back through a unique Linux abstract Unix socket for each rule. Socket names are never reused, even after rule deletion or service restart. Leftover reverse mappings on a device therefore cannot enter another session through TCP port reuse. A failed deletion returns ADB FAIL, stops new connections, and retries in the background. If deletion took effect but its response was lost, the gateway queries the device's rules to confirm the result.

Listening rules themselves hold no lock. Each actual data connection holds the corresponding read lock or lease-based access. Disconnection, Ctrl-C, authorization revocation, or certificate expiration closes the remote session and its rules. Managed normal tunnels automatically establish a new session after recoverable failures while retaining the main local ADB port. Forward/reverse rules are not restored and ADB commands are not replayed.

Authorization revocation or lease termination cancels backend I/O and waits for request handlers to finish and backend connections to close before handing over the device lock. Failed reverse cleanup retains state and retries while the device generation remains valid. Once the generation changes, only the old entry point is reclaimed, preventing deletion of mappings belonging to a new device session.

## Exclusive leases

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

## REST and tunnel API

All endpoints require mTLS. Encode path parameters as URL path segments. Group configuration is managed in YAML; the API does not provide endpoints to modify group membership.

| Method and path | Description |
| --- | --- |
| GET /v1/groups | List authorized groups, static groups before automatic groups |
| GET /v1/groups/{group} | Group name and effective device membership |
| GET /v1/groups/{group}/devices | Group device and lock states, including disconnected members |
| GET /v1/groups/{group}/devices/{device} | Device state and mTLS tunnel endpoint description |
| POST /v1/groups/{group}/devices/{device}/write-leases | Body accepts optional wait_seconds and ttl_seconds |
| GET /v1/groups/{group}/devices/{device}/write-leases/{lease} | Query your own lease |
| PATCH /v1/groups/{group}/devices/{device}/write-leases/{lease} | Set expires_at |
| DELETE /v1/groups/{group}/devices/{device}/write-leases/{lease} | Release idempotently |
| POST /v1/groups/{group}/tunnels | Body {} or {"lease_id":"..."}; create a pending tunnel |
| GET /v1/groups/{group}/tunnels/{tunnel} | Query your own pending/connecting/active/closed tunnel |
| DELETE /v1/groups/{group}/tunnels/{tunnel} | Close idempotently; retain terminal state for one hour |
| GET /v1/groups/{group}/tunnels/{tunnel}/control | Upgrade to a WebSocket control connection |
| POST /v1/groups/{group}/tunnels/{tunnel}/streams | Body {"kind":"adb"} or {"kind":"forward","rule_id":"..."} |
| GET /v1/groups/{group}/tunnels/{tunnel}/streams/{stream} | Upgrade to a binary WebSocket data stream that accepts a single connection |

Tunnels and data streams return `id` and `path`, plus a Location header on successful creation. An ID does not grant authorization: resources are bound to an identity, group, tunnel, and purpose. Missing resources or resources outside the caller's permissions return 404; unauthorized identities return 403; state conflicts or stream limits return 409; tunnel count limits return 429. Request bodies are limited to 4 KiB, and unknown fields or multiple JSON values are rejected.

Control messages use version 1 JSON: `version,id,op,rule,previous,stream,port,error`. After the server sends ready, the client handles bind, activate, remove, remember, forget, and reverse, replying with reply and the same id. Forward binding and activation are separate phases, so a failed listener does not overwrite an existing rule. The server creates reverse data streams and notifies the owning client through control; connections can reach only the loopback target of an acknowledged rule.

Each data WebSocket uses binary messages without compression. Each local ADB or forwarding connection has a dedicated data WebSocket, while control messages travel separately. Client WebSockets use HTTP/1.1. Reverse proxies must support WebSocket Upgrade and long-lived connections while preserving end-to-end mTLS authentication. Identity headers supplied by a proxy are not trusted.

Tunnels and data streams must connect within 10 seconds of creation. Control messages are limited to 64 KiB, with at most 64 pending requests and a 10-second response timeout. Heartbeats are sent every 15 seconds with a 10-second wait. Each tunnel supports at most 128 data streams; each identity supports at most 32 tunnels that have not been closed. Long-lived data streams have no fixed total duration but remain subject to certificate validity, authorization, device generation, and lease constraints.

## Reloading, migration, and validation

SIGHUP reloads only device membership in existing groups, identity authorization, the read wait timeout, and the queue limit. Group order may also change. Revocation closes affected tunnels and streams and cleans up their rules; other authorized users' connections to shared devices continue working.

Changes to listening ports, backend settings, TLS/PKI configuration, or `auto_groups` require a full restart. Adding, deleting, or renaming static groups also requires a full restart. A reload containing any such change rejects the entire new configuration and retains the old one, without applying a subset. Use `systemctl restart adbgroup` for an explicit full restart; existing proxy connections, leases, and gateway rules are not retained.

This is a breaking upgrade: `lease_ssh`, group `listen`, `forward_min`, `forward_max`, and the `ssh-config` command have been removed. Old fields are strictly rejected. After migration, administrators should remove obsolete system SSH group accounts and restriction snippets; the program never modifies the system automatically. Existing CA and client certificates remain valid and do not need to be reissued. The service continues to use one Linux daemon account and creates no accounts for users or teams.

The deployment example `deploy/adbgroup.service` uses the `adbgroup` service account and state directory `/var/lib/adbgroup`. Place the configuration at `/etc/adbgroup/config.yaml` and set `pki.directory: /var/lib/adbgroup/pki`. Configure USB udev permissions and an appropriate `plugdev` group for your distribution.

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Shell integration tests require bash, zsh, and `script` on PATH, plus permission to listen on local sockets. On Linux, `script` is provided by util-linux; macOS includes it. Missing tools fail these tests immediately.

End-to-end tests run real mTLS, HTTP Upgrade, and WebSocket over in-memory connections. They cover ADB data, client-side forward/reverse, identity revocation, lease binding, and cleanup after disconnection, without requiring OS TCP listener permissions. Native adb tests require adb and permission to listen on TCP; otherwise, they are skipped. Real USB acceptance testing requires a shared device, two devices in the same group, and a device in another group. Run concurrent shells, file transfers, and forwarding, then unplug and reconnect one device to confirm that the others stay connected.

Without external dependencies, you can validate the core and PKI separately with Go 1.22+:

```sh
GOTOOLCHAIN=local go test -modfile=test/core.mod -race ./internal/gateway ./internal/pki
```

If a recent macOS version with Go 1.22 reports missing LC_UUID, add `-ldflags=-linkmode=external`. Use Go 1.26+ for production builds and full acceptance testing.
