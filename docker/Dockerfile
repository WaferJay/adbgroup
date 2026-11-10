# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/adbgroup ./cmd/adbgroup

FROM debian:trixie-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends adb ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 adbgroup \
    && useradd --uid 10001 --gid adbgroup --home-dir /var/lib/adbgroup \
        --no-create-home --shell /usr/sbin/nologin adbgroup \
    && install -d -o adbgroup -g adbgroup -m 0700 \
        /var/lib/adbgroup /etc/adbgroup

COPY --from=build /out/adbgroup /usr/local/bin/adbgroup

ENV HOME=/var/lib/adbgroup

USER adbgroup:adbgroup
WORKDIR /var/lib/adbgroup

EXPOSE 8443

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/adbgroup"]
CMD ["serve", "-config", "/etc/adbgroup/config.yaml"]
