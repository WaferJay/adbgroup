# Client guide

[Project overview](../README.md) · [简体中文](client.zh-CN.md)

## Installation options

To install or upgrade the latest release on macOS or Linux, use the [quick start](../README.md#install-adg). To pin a release or change the install directory, pass `--tag` or `--bin-dir` to the installer.

```sh
curl -fsSL https://raw.githubusercontent.com/WaferJay/adbgroup/main/install.sh | sh -s -- --tag v0.1.0-beta.4 --bin-dir "$HOME/bin"
```

For a manual install, choose the archive for your OS and architecture, verify it against `checksums.txt`, and place the extracted `adg` on your `PATH`. [Release downloads](https://github.com/WaferJay/adbgroup/releases)

## Install the adg skill

**Codex CLI** — run in your terminal.

```sh
codex plugin marketplace add WaferJay/adbgroup
codex plugin add adg@adbgroup
```

**Claude Code** — run in a Claude Code session.

```text
/plugin marketplace add WaferJay/adbgroup
/plugin install adg@adbgroup
```

Start a new agent session. You can ask “Install adg for me” to run the same installer. For your first connection, import your administrator's credential pack with `adg profile import client.adgpack`, then ask “Use adg to list the device groups I can access.”

## Credential profiles

Get `client.adgpack` from the gateway administrator over a private channel such as SCP. The pack contains the user's unencrypted private key. The suggested profile name is the original export directory name.

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

The first profile becomes the default on a fresh installation. Later imports coexist. Renewals keep existing tunnels and their reconnects on their original credential version. A changed gateway or identity needs a separate name. Updating server CA trust requires interactive confirmation of the displayed SHA-256 fingerprints. The local store under `~/.config/adbgroup/profiles/` retains earlier credential versions for existing sessions.

Issuance uses configured `pki.server_names` covered by the actual HTTPS certificate and the port from `api.listen`. Without configured names it uses concrete certificate SANs. One candidate is automatic. For multiple candidates the importer offers a menu. For scripts, missing candidates or port mappings, pass `-server https://host:port` when issuing or importing. For a separate HTTPS CA, pass `-server-ca gateway-ca.pem` when issuing or repacking.

To issue or repack credentials on the gateway, see [server certificates](server.md#certificates).

### Existing YAML configurations

Existing clients continue to use `~/.config/adbgroup/tunnel.yaml` until a profile is selected as default. Explicit `-f` selects a YAML configuration and is mutually exclusive with `-p`. Example legacy client YAML:

```yaml
server: https://adb-host.example:8443
# Optional; defaults to the directory containing tunnel.yaml
credentials: /path/to/run/clients/alice
```

When upgrading, remove the top-level `group:` field from existing client YAML files. Pass the group on the command line or select one with `adg use GROUP` for device queries.

The `credentials` directory uses the fixed filenames produced by the `cert` command: `client.pem`, `client.key`, and `ca.pem`. You can also place these files next to `tunnel.yaml` and omit `credentials`.

To import an existing YAML configuration as a named profile, run the following command.

```sh
adg profile import -f staging.yaml -name staging
```

## List groups and devices

`groups/g` and `devices/d` query the gateway's REST API directly with the client configuration and mTLS credentials.

```sh
adg g                  # List authorized groups (NAME) and their device counts (DEVICES)
adg d                  # Devices, connection states, and lock states in the selected group
adg d team-b           # Devices in a specific group, including offline devices
adg g --json            # Group list as JSON
adg d @devices --json  # Inventory for the group named "devices" as JSON
```

Query commands accept `-s/-d/-f`, their long forms, and `-cert/-key/-ca`. Command-line flags override the configuration file. Failed queries return a nonzero exit code, errors go to stderr, and JSON results go separately to stdout.

## Run adb for a group

With Android Platform Tools' `adb` on your PATH, put the group name before the adb arguments. Each invocation selects its own group and works from a fresh terminal, a script, or CI without shell initialization or exports.

```sh
adg team-a devices -l                     # List devices in team-a
adg team-b -s USB_SERIAL_B shell          # Open a shell on one device in team-b
adg -f staging.yaml team-a devices        # Use a different client config
adg @devices devices -l                   # The group itself is named "devices"
```

Put client connection options before the group. For example, `-s` before the group selects the gateway server, and `-s` after the group selects an adb serial. Built-in commands and aliases take precedence in the first position. Prefix a group name with `@` to select it explicitly. Scripts can use `adg "@$group" devices -l` for any group name, including a command or alias. To address a group named `@lab`, write `@@lab`. Existing scripts can continue to use `-g GROUP` or `-group GROUP`.

The daemon reuses a normal tunnel for the server, credentials, and group, or opens one on an available loopback port. A one-off command uses its own client configuration and group, even inside a shell with another gateway or group selected. The native adb process gets the original argument boundaries, standard input, output, and error streams. Its exit status is the command's exit status. The tunnel supplies the ADB server endpoint, so native adb server options `-H`, `-P`, and `-L` conflict with group selection. For a write lease, use `adg t GROUP -w <lease-id>`.

## Select a group for the current terminal

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

### Wait for a device

Native `adb -s USB_SERIAL_A wait-for-device` is supported. It waits while the device is disconnected, offline, or unauthorized, then returns when the device enters the `device` state. Other supported commands are `wait-for-recovery`, `wait-for-rescue`, `wait-for-sideload`, `wait-for-bootloader`, and `wait-for-disconnect`. Waiting observes only devices in the current group, takes no read lock, and is not subject to the read-lock wait timeout. With multiple devices, select a target using `-s` or `-t`. For USB devices, `wait-for-disconnect` completes when the device disappears from the list; going offline alone does not count as a disconnect. Ctrl-C, tunnel closure, or authorization revocation ends the wait.

## Command help

`adg --help` (also `-h` or `-help`) lists commands. Use `adg <command> -help` to see flags and defaults, for example `adg t -help`, `adg t stop -help`, or `adg daemon -help`. Help works before configuration and exits without starting any services.

To connect Appium or set up port forwarding, see [Tunnels and port forwarding](tunnels.md).
