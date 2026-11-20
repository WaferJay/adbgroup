# Server administration

[Project overview](../README.md) · [简体中文](server.zh-CN.md)

## Start the gateway

On the Linux USB host, install Android SDK Platform Tools and give the service account access to the USB devices. Install the Linux release of `adg` or build it from the repository root with Go 1.26+.

```sh
go build -o bin/adg ./cmd/adbgroup
export PATH="$PWD/bin:$PATH"
```

Copy the example configuration from the repository or an extracted release archive.

```sh
cp config.example.yaml config.yaml
```

Set `backend.binary` to the host’s `adb` path, assign USB serials to `groups`, and grant groups to certificate identities in `identities`. Set `pki.server_names` to the DNS names or IPs clients will use before starting the gateway for the first time. [Example configuration](../config.example.yaml)

```sh
adg check -config config.yaml
adg serve -config config.yaml
```

Each user’s client certificate authenticates both API calls and tunnels. Teams are virtual groups managed in the gateway configuration. They require no Linux accounts, system sshd, or SSH keys.

## USB backend

The REST API and WebSocket tunnels share one HTTPS endpoint. When `backend.address` is a
numeric loopback address such as `127.0.0.1:5038`, the ADB server listens only on loopback.
A bare port such as `5038` makes it listen on all interfaces for container port forwarding;
the gateway still connects internally through `127.0.0.1:5038`. The raw ADB port bypasses
mTLS, group authorization, and device locks. Map it explicitly to the host's loopback
address only; do not expose it to remote users.

`backend.manage: true` starts a persistent ADB server on a dedicated port and refuses to start if another process already uses that port. Unplugging or reconnecting one device does not restart the backend, so other devices keep running. An ADB server failure still interrupts all device connections.

USB enumeration supports both the native backend's `usb:` paths and the bus/port paths without that prefix used by some libusb versions. Network devices and emulators remain excluded.

## Deploy with systemd or Docker

The example systemd unit runs `/usr/local/bin/adg` as `adbgroup`, reads `/etc/adbgroup/config.yaml`, and uses `/var/lib/adbgroup` for state. Set `pki.directory: /var/lib/adbgroup/pki` in the configuration file. Give the service account access to the USB device nodes through udev and the appropriate `plugdev` group. [systemd unit](../deploy/adbgroup.service) · [udev rules](../docker/70-android-adb.rules)

```sh
docker pull ghcr.io/waferjay/adbgroup:latest
```

For a Linux Docker host, the Compose guide covers USB permissions, persistent credentials, startup, and reloads. [Docker deployment (中文)](../docker/README.md)

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

### Issue and distribute credential packs

With managed PKI, each `adg serve` startup provisions client PEM files and a `client.adgpack` for new identities in `identities`. For `spiffe://adbgroup/alice`, the default pack path is `run/pki/clients/alice/client.adgpack`. Transfer it privately to Alice, who can run `adg profile import client.adgpack`. Other identity URIs have separate export directories. The issuance log records each pack path.

On startup, the server rebuilds missing packs and atomically replaces packs whose advertised HTTPS addresses have changed, using the original PEM files. After changing the `api.listen` port, restart the server and distribute the refreshed pack. Clients with a profile for the old address can import the pack under a new name with `adg profile import -name alice-new client.adgpack`.

SIGHUP reloads authorization. Added identities receive their packs on the next startup. With external PKI, administrators issue client credentials with their signing CA.

Administrators can also issue user credentials explicitly:

```sh
adg cert -config config.yaml -user alice -out run/clients/alice
# Custom identity and validity period
adg cert -config config.yaml \
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

To package previously issued PEM files, run the following command on the gateway.

```sh
adg cert pack -config config.yaml -credentials run/clients/alice -bundle alice.adgpack
```

On the client, import the pack into a profile. [Credential profiles](client.md#credential-profiles)

## Reloading and migration

SIGHUP reloads only device membership in existing groups, identity authorization, the read wait timeout, and the queue limit. Group order may also change. Revocation closes affected tunnels and streams and cleans up their rules; other authorized users' connections to shared devices continue working.

Changes to listening ports, backend settings, TLS/PKI configuration, or `auto_groups` require a full restart. Adding, deleting, or renaming static groups also requires a full restart. A reload containing any such change rejects the entire new configuration and retains the old one, without applying a subset. Use `systemctl restart adbgroup` for an explicit full restart; existing proxy connections, leases, and gateway rules are not retained.

This is a breaking upgrade: `lease_ssh`, group `listen`, `forward_min`, `forward_max`, and the `ssh-config` command have been removed. Old fields are strictly rejected. After migration, administrators should remove obsolete system SSH group accounts and restriction snippets; the program never modifies the system automatically. Existing CA and client certificates remain valid and do not need to be reissued. The service continues to use one Linux daemon account and creates no accounts for users or teams.

The executable is named `adg`; the project, archive names, configuration paths,
service account, and certificate identities retain the name `adbgroup`. When
upgrading from an `adbgroup` executable, update scripts and shell initialization
to call `adg`.
