#!/bin/sh
# mkrootfs PACKAGE... — install Alpine packages for the target architecture
# ($TARGETARCH) into /rootfs, on the build platform. Nothing of the target
# architecture is executed, so no QEMU emulation is needed. Used by both
# images in the Dockerfile.
set -eu

case "${TARGETARCH}" in
  amd64) APK_ARCH=x86_64 ;;
  arm64) APK_ARCH=aarch64 ;;
  *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;;
esac

mkdir -p /rootfs/etc/apk/keys
cp /usr/share/apk/keys/${APK_ARCH}/*.pub /rootfs/etc/apk/keys/
cp /etc/apk/repositories /rootfs/etc/apk/repositories
# Alpine edge as a tagged repository: only packages requested as
# "name@edge" (and what they need) are taken from it.
for repo in main community; do
    echo "@edge https://dl-cdn.alpinelinux.org/alpine/edge/${repo}" >> /rootfs/etc/apk/repositories
done
apk add --root /rootfs --arch ${APK_ARCH} --initdb --no-cache --no-scripts \
    alpine-baselayout alpine-release busybox busybox-binsh musl-utils "$@"

# busybox normally creates its applet links in a post-install script,
# which is skipped here; the applet list is the same for every architecture.
# Real commands installed by the packages above take precedence.
for applet in $(busybox --list-full); do
    mkdir -p "/rootfs/$(dirname "${applet}")"
    [ -e "/rootfs/${applet}" ] || [ -L "/rootfs/${applet}" ] || ln -s /bin/busybox "/rootfs/${applet}"
done
rm -rf /rootfs/var/cache/apk/* /rootfs/etc/apk/cache
