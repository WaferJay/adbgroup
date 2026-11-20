---
name: adg
description: Use adg to import client credentials, list authorized device groups, check device and lock status, and run adb commands on Android devices through the ADB Group Gateway.
---

# adg

## Import a credential pack

Use `adg` v0.1.0-beta.4 or later. The `.adgpack` file from your administrator contains your client certificate and private key. Import it from a local path on the machine where you run `adg`.

```sh
adg profile import -name lab ~/Downloads/client.adgpack
adg profile list     # Check the profile name, gateway, identity, and certificate expiry
adg groups -p lab    # Verify the connection by listing groups this profile can access
```

`lab` is the local profile name. Omit `-name lab` to use the name stored in the pack. On a fresh install, the first profile becomes the default. If the pack lists multiple gateways, select one by number at the prompt or pass `-server URL` before the file path to specify the HTTPS address.

## List groups and devices

- `adg groups` lists the groups your current identity can access.
- `adg devices team-a` shows each device's serial number, connection state, and read/write lock status in `team-a`. Append `--json` for structured output.

## Run adb commands

For a one-off command, make sure `adb` is on your `PATH`. Replace `team-a` and `USB_SERIAL_A` with the target group and device serial number.

```sh
adg team-a -s USB_SERIAL_A shell getprop ro.product.model
```

Place gateway options such as `-p PROFILE` before the group name and adb options such as `-s SERIAL` after it. If the group name matches an adg command or alias, prefix it with `@`, as in `adg @devices devices -l`.

To run several native `adb` commands in the same terminal, initialize shell integration with `eval "$(command adg shell-init zsh)"` (use `bash` in place of `zsh` for Bash). Select the group with `adg use team-a`. When finished, run `adg unuse` to restore the previous ADB socket.
