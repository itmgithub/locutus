# syntax=docker/dockerfile:1
#
# Multi-architecture images (linux/amd64 and linux/arm64), each pushed to
# its own repository:
#   * target "locutus" (the default): the Locutus web application;
#   * target "client": the Locutus client container, a repository server
#     and/or a client whose volumes Locutus backs up (see client/).
#
# Nothing in this file executes binaries of the target architecture, so no
# QEMU emulation is needed:
#   * the Go binaries are cross-compiled on the build platform (CGO disabled,
#     SQLite is the pure Go modernc.org/sqlite driver);
#   * the runtime packages are installed with "apk --arch" into a separate
#     root file system (docker/mkrootfs.sh), also on the build platform.
#
# Build and push both architectures:
#   docker buildx build --platform linux/amd64,linux/arm64 -t itmon.nl/locutus --push .
#   docker buildx build --platform linux/amd64,linux/arm64 --target client -t itmon.nl/locutus-client --push .

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.24

# ── Build the Go binaries ─────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /app

# Only what the build needs, so that other changes in the directory (such as
# the image files of build.sh) do not rebuild the binaries. The Go caches are
# kept between builds, so a change only compiles the packages it affects.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY main.go ./
COPY cmd ./cmd
COPY internal ./internal
# For the build number: the current commit, without the rest of .git (which
# changes with every git command). The wildcards allow a build without .git.
COPY .gi[t]/HEA[D] .gi[t]/packed-ref[s] ./.git/
COPY .gi[t]/ref[s]/head[s] ./.git/refs/heads/

# Build number: the BUILD build-arg when given, otherwise the first 5
# characters of the current git commit. The version is in internal/version.
ARG BUILD=""
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    commit="$(cat .git/HEAD 2>/dev/null)" && \
    case "${commit}" in \
        "ref: "*) ref="${commit#ref: }" && \
                  commit="$(cat ".git/${ref}" 2>/dev/null || sed -n "s| ${ref}\$||p" .git/packed-refs 2>/dev/null)" ;; \
    esac && \
    BUILD="${BUILD:-$(echo "${commit}" | cut -c1-5)}" && BUILD="${BUILD:-dev}" && \
    echo "Building Locutus build ${BUILD} for ${TARGETOS}/${TARGETARCH}" && \
    export CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} && \
    LDFLAGS="-s -w -X itmon.nl/locutus/internal/version.Build=${BUILD}" && \
    go build -trimpath -ldflags="${LDFLAGS}" -o /out/locutus . && \
    go build -trimpath -ldflags="${LDFLAGS}" -o /out/locutus-client ./cmd/locutus-client

# ── Runtime root file systems for the target architecture ────────────────────
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS rootfs-base
COPY docker/mkrootfs.sh /usr/local/bin/mkrootfs

# The Alpine packages below are installed again when PACKAGES_DATE changes,
# instead of being taken from the build cache. build.sh sets it to the date of
# today, so the first build of a day gets the latest security fixes and later
# builds that day reuse them. Without it, rebuilding the images would keep the
# packages of the first build.
ARG PACKAGES_DATE=""
RUN echo "${PACKAGES_DATE}" > /etc/packages-date

# Packages tagged "@edge" come from Alpine edge (see docker/mkrootfs.sh)
# because the stable release has no fix yet for their known CVEs:
#   * sqlite-libs >= 3.54.0 (heap overflows / use-after-free in FTS3/FTS5,
#     R-Tree, session extension; via python3);
#   * openssh >= 10.6 (CVE-2026-1065xx);
#   * py3-msgpack >= 1.2.1 (CVE-2026-57585, CVE-2026-114613) and the
#     borgbackup build that accepts it;
#   * musl >= 1.2.6-r6 (CVE-2026-107891, regcomp);
#   * sudo >= 1.9.17_p2-r4 (CVE-2026-96512; client only).
# Drop the tags once the stable Alpine release ships these versions.
ARG EDGE_PACKAGES="musl@edge musl-utils@edge sqlite-libs@edge py3-msgpack@edge borgbackup@edge"

FROM rootfs-base AS rootfs-locutus
ARG TARGETARCH
ARG EDGE_PACKAGES
RUN mkrootfs ca-certificates-bundle tzdata openssh-client-default@edge ${EDGE_PACKAGES}

# The client also needs the SSH server, sudo, and GNU tar, stat and xargs
# for restores.
FROM rootfs-base AS rootfs-client
ARG TARGETARCH
ARG EDGE_PACKAGES
RUN mkrootfs ca-certificates-bundle tzdata openssh-server@edge openssh-client-default@edge ${EDGE_PACKAGES} \
        sudo@edge tar coreutils findutils

# ── Locutus client image ──────────────────────────────────────────────────────
FROM scratch AS client

COPY --from=rootfs-client /rootfs/ /
COPY --from=builder /out/locutus-client /usr/local/bin/locutus-client
COPY --chmod=755 contrib/locutus-borg-wrapper.sh /usr/local/bin/locutus-borg-wrapper

ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

EXPOSE 22
VOLUME ["/config"]

ENTRYPOINT ["/usr/local/bin/locutus-client"]

# ── Locutus image (the default target) ────────────────────────────────────────
FROM scratch AS locutus

COPY --from=rootfs-locutus /rootfs/ /
COPY --from=builder /out/locutus /app/locutus

ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LOCUTUS_DATA_DIR=/data \
    LOCUTUS_LISTEN_ADDR=:2224

WORKDIR /app
EXPOSE 2224
VOLUME ["/data"]

CMD ["/app/locutus"]
