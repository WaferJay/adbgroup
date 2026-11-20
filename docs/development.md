# Development and releases

[Project overview](../README.md) · [简体中文](development.zh-CN.md)

## Run the tests

Run the full suite from the repository root with Go 1.26+.

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

## Docker images

The [Docker image workflow](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/docker.yml) builds Linux images for
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

Set the GHCR package visibility to public after the first publication to allow anonymous pulls. [Docker deployment (中文)](../docker/README.md)

## Binary releases

Pushing a semantic version tag such as `v0.1.0` or `v0.1.0-beta.1` runs the
[CI and release workflow](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/docker.yml). After tests and Docker
image publication succeed, the [release job](https://github.com/WaferJay/adbgroup/blob/main/.github/workflows/release.yml) uses
[GoReleaser](https://github.com/WaferJay/adbgroup/blob/main/.goreleaser.yaml) to publish a GitHub Release containing:

- Linux and macOS binaries for `amd64` and `arm64`, packaged as
  `adbgroup_<version>_<os>_<arch>.tar.gz`;
- `checksums.txt` with SHA-256 hashes;
- release notes generated from commits since the previous tag.

Each archive includes both READMEs, the guides in `docs/`, example server/client configuration, the
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

The snapshot archives and checksums are written to `dist/`.
