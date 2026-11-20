# Tunnels and port forwarding

[Project overview](../README.md) · [简体中文](tunnels.zh-CN.md)

```text
Native adb / Appium → local adg tunnel → HTTPS (mTLS) → Linux gateway → ADB server → USB device
```

## Start a local tunnel

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
adg t stop TUNNEL_ID   # Use the ID from adg t list to stop and remove the tunnel

adg daemon status
adg daemon start      # Optional; t starts it automatically
adg daemon stop       # Exit after all tunnels finish cleanup

adg t team-a --no-daemon      # Run in the foreground; exit with Ctrl-C
```

A successful startup prints the tunnel ID and `export ADB_SERVER_SOCKET=tcp:127.0.0.1:<port>`. Set that environment variable in the terminal running adb or Appium to select the tunnel. Submitting the same parameters again reuses an existing tunnel. Using different settings for the same listening address causes a conflict; stop the old tunnel first.

The tunnel authenticates with the remote server before opening the local listener and accepts only numeric loopback addresses. The local port is the client's trust boundary: any process on the same machine that can connect to it can use it. The server reauthorizes every data channel against the certificate identity and selected group. Port numbers, source IPs, and tunnel IDs do not replace identity.

## Connection settings

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

## Daemon and reconnection

After a managed normal tunnel has connected successfully, server restarts and network outages change its state to `reconnecting`. The daemon retains the local port, group, and local tunnel ID, reloads credentials, and establishes a fresh authenticated remote session. New ADB requests receive `FAIL` while unavailable; existing connections end. Once connected again, subsequent ADB commands work through the same socket without another `use`. Forward/reverse rules must be recreated, and commands are never replayed.

Retries use exponential backoff from 1 to 30 seconds with ±20% jitter; 30 seconds continuously online resets the backoff. Each connection attempt is limited to 30 seconds. Network failures, HTTP 5xx/429/408, and temporarily missing groups (404) are retried. Certificate, trust, protocol, or explicit authorization failures stop automatic retries and show `failed`; the local port remains reserved until the tunnel is stopped. Fix the cause and repeat `use` or `t` to retry. Initial setup failures, foreground `t GROUP --no-daemon`, and write tunnels using `t GROUP -w` require explicit retry. The daemon does not persist tunnel configurations; submit them again after the daemon itself restarts.

The daemon supports macOS and Linux. Its control socket is `~/.config/adbgroup/daemon/control.sock`, with directory permissions `0700` and socket permissions `0600`. Logs go to `daemon.log` in the same directory. An incompatible local control protocol version produces an error; stop the daemon with the old binary before starting it with the new one. The initial release does not install a system service or configure startup at boot.

When upgrading to daemon control protocol v3, run `adg daemon stop` with the old binary before upgrading, then submit your tunnels again with the new binary.

## Forward and reverse ports

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

To reserve a device across groups and open a write tunnel, see [Exclusive leases](api.md#exclusive-leases).
