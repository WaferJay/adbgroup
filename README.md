# ADB Group Gateway

**English** | [简体中文](README.zh-CN.md)

`adg` provides a local ADB tunnel to Android devices connected by USB to a shared Linux host. The gateway authenticates users with mutual TLS (mTLS) and limits access to their assigned device groups. An exclusive lease reserves a physical device across all its groups.

## Install adg

On macOS or Linux, run this command to install or upgrade to the newest published release, including prereleases.

```sh
curl -fsSL https://raw.githubusercontent.com/WaferJay/adbgroup/main/install.sh | sh
```

The installer verifies the SHA-256 checksum and places `adg` in `~/.local/bin`. Add that directory to your `PATH`, along with Android SDK Platform Tools if you run native `adb` commands.

[Manual installation and version pinning](docs/client.md#installation-options) · [Install the adg skill for Codex CLI or Claude Code](docs/client.md#install-the-adg-skill)

## Connect to a device

Get `client.adgpack` from your gateway administrator over a private channel. It contains your client certificate and private key. Import it, then use a group and an online device serial from the listings in place of `team-a` and `USB_SERIAL_A`.

```sh
adg profile import client.adgpack
adg groups
adg devices team-a
adg team-a -s USB_SERIAL_A shell getprop ro.product.model
```

## Run a Linux gateway

On the USB host, install `adg` and Android SDK Platform Tools and give the service account access to the USB devices. Copy `config.example.yaml` to `config.yaml`. Set `backend.binary` to the local `adb` path, enter your device serials and user identities, and set `pki.server_names` to the hostname or IP clients will use. [Example configuration](config.example.yaml)

```sh
adg check -config config.yaml
adg serve -config config.yaml
```

With the example configuration, the first startup creates Alice’s credential pack at `run/pki/clients/alice/client.adgpack`. Transfer it privately to Alice for import.

## Documentation

| Guide | Topics |
| --- | --- |
| [Client guide](docs/client.md) | Installation options, agent skill, credential profiles, device queries, and shell integration |
| [Tunnels and port forwarding](docs/tunnels.md) | Connecting ADB clients, the local daemon, reconnection, and port forwarding |
| [Server administration](docs/server.md) | USB access, systemd, automatic groups, certificates, and configuration reloads |
| [Docker deployment (中文)](docker/README.md) | Compose, USB permissions, and persistent credentials |
| [Gateway API](docs/api.md) | Exclusive leases, REST endpoints, and the WebSocket tunnel protocol |
| [Development and releases](docs/development.md) | Tests, container images, and binary releases |

## License

This project uses [The Unlicense](LICENSE).
