# syntax=docker/dockerfile:1
#
# Multi-architecture image (linux/amd64 and linux/arm64) for Locutus.
#
# Nothing in this file executes binaries of the target architecture, so no
# QEMU emulation is needed:
#   * the Go binary is cross-compiled on the build platform (CGO disabled,
#     SQLite is the pure Go modernc.org/sqlite driver);
#   * the runtime packages (borgbackup, openssh-client, ...) are installed
#     with "apk --arch" into a separate root file system, also on the build
#     platform.
#
# Build for both architectures:
#   docker buildx build --platform linux/amd64,linux/arm64 -t itmon.nl/locutus .

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

# ── Build the Go binary ───────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /app

# git is needed to determine the build number (short commit id)
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build number: the BUILD build-arg when given, otherwise the first 5
# characters of the current git commit.
ARG BUILD=""
ARG TARGETOS
ARG TARGETARCH
RUN git config --global --add safe.directory /app && \
    BUILD="${BUILD:-$(git rev-parse --short=5 HEAD 2>/dev/null || echo dev)}" && \
    echo "Building Locutus build ${BUILD} for ${TARGETOS}/${TARGETARCH}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.Build=${BUILD}" -o /out/locutus .

# ── Runtime root file system for the target architecture ─────────────────────
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS rootfs

ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) APK_ARCH=x86_64 ;; \
      arm64) APK_ARCH=aarch64 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /rootfs/etc/apk/keys; \
    cp /usr/share/apk/keys/${APK_ARCH}/*.pub /rootfs/etc/apk/keys/; \
    cp /etc/apk/repositories /rootfs/etc/apk/repositories; \
    apk add --root /rootfs --arch ${APK_ARCH} --initdb --no-cache --no-scripts \
        alpine-baselayout alpine-release busybox busybox-binsh musl-utils \
        ca-certificates-bundle tzdata openssh-client-default borgbackup; \
    # busybox normally creates its applet links in a post-install script,
    # which is skipped here; the applet list is the same for every architecture.
    for applet in $(busybox --list-full); do \
      mkdir -p "/rootfs/$(dirname "${applet}")"; \
      [ -e "/rootfs/${applet}" ] || [ -L "/rootfs/${applet}" ] || ln -s /bin/busybox "/rootfs/${applet}"; \
    done; \
    rm -rf /rootfs/var/cache/apk/* /rootfs/etc/apk/cache

# ── Runtime image ──────────────────────────────────────────────────────────────
FROM scratch

COPY --from=rootfs /rootfs/ /
COPY --from=builder /out/locutus /app/locutus

ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LOCUTUS_DATA_DIR=/data \
    LOCUTUS_LISTEN_ADDR=:3000

WORKDIR /app
EXPOSE 3000
VOLUME ["/data"]

CMD ["/app/locutus"]
