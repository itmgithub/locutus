#!/bin/sh
# build.sh — build the Locutus and Locutus client images as image files, one
# per image and architecture, to copy to another machine and load there with
# "docker load -i <file>".
#
#   ./build.sh                     both images, amd64 and arm64
#   ./build.sh -t client -a arm64  only the client, only arm64
#
# Options:
#   -t TARGET  locutus or client; repeat for both (default: both)
#   -a ARCH    amd64 or arm64; repeat for both (default: both)
#   -o DIR     directory for the image files (default: the current directory)
#   -p PREFIX  image name prefix (default: itmon.nl)
#   -b BUILD   build number (default: the start of the git commit)
#
# The base images are pulled again (--pull) and the Alpine packages are
# installed again at the first build of a day (PACKAGES_DATE, see the
# Dockerfile), so a new build has the latest security fixes.
#
# Every image is tagged "latest" and with the version from
# internal/version/version.go, for example itmon.nl/locutus-client:v1.2.
# The files are called <image>-<arch>.tar, for example locutus-client-arm64.tar.
set -eu

cd "$(dirname "$0")"

targets=""
arches=""
outdir="."
prefix="itmon.nl"
build=""

usage() {
    sed -n '2,/^set -eu/p' "$0" | sed '$d; s/^# \{0,1\}//'
    exit "${1:-0}"
}

while getopts "t:a:o:p:b:h" opt; do
    case "$opt" in
        t) case "$OPTARG" in
               locutus|client) targets="$targets $OPTARG" ;;
               *) echo "build.sh: unknown target $OPTARG (use locutus or client)" >&2; exit 2 ;;
           esac ;;
        a) case "$OPTARG" in
               amd64|arm64) arches="$arches $OPTARG" ;;
               *) echo "build.sh: unknown architecture $OPTARG (use amd64 or arm64)" >&2; exit 2 ;;
           esac ;;
        o) outdir="$OPTARG" ;;
        p) prefix="${OPTARG%/}" ;;
        b) build="$OPTARG" ;;
        h) usage ;;
        *) usage 2 ;;
    esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || usage 2

targets="${targets:-locutus client}"
arches="${arches:-amd64 arm64}"
version="$(sed -n 's/^var Version = "\(.*\)"$/\1/p' internal/version/version.go)"
[ -n "$version" ] || { echo "build.sh: no version found in internal/version/version.go" >&2; exit 1; }
build="${build:-$(git rev-parse --short=5 HEAD 2>/dev/null || echo dev)}"
mkdir -p "$outdir"

echo "Locutus $version (Build: $build): $targets for $arches"
for target in $targets; do
    case "$target" in
        locutus) image=locutus ;;
        client)  image=locutus-client ;;
    esac
    for arch in $arches; do
        file="$outdir/$image-$arch.tar"
        echo "==> $prefix/$image:$version for linux/$arch -> $file"
        docker buildx build --platform "linux/$arch" --target "$target" --pull \
            --build-arg "BUILD=$build" --build-arg "PACKAGES_DATE=$(date -u +%F)" \
            -t "$prefix/$image:latest" -t "$prefix/$image:$version" \
            -o "type=docker,dest=$file" .
    done
done

echo "Done. Load an image on the target machine with: docker load -i <file>"
