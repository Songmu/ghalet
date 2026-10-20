#!/bin/sh
set -eu

if [ "$#" -lt 4 ]; then
  echo "Usage: $0 VERSION ARCHIVE_DIR OUTPUT_DIR BINARY_DIR [ARCH...]" >&2
  exit 1
fi

version=$1
archive_dir=$(cd "$2" && pwd)
mkdir -p "$3" "$4"
output_dir=$(cd "$3" && pwd)
binary_dir=$(cd "$4" && pwd)
shift 4
if [ "$#" -eq 0 ]; then
  set -- amd64 arm64
fi
cd "$(dirname "$0")/.."
mkdir -p "$binary_dir/licenses"
install -m 0644 LICENSE CREDITS "$binary_dir/licenses/"

staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT HUP INT TERM
export VERSION="${version#v}"

for arch do
  case "$arch" in
    amd64|arm64) ;;
    *) echo "Unsupported package architecture: $arch" >&2; exit 1 ;;
  esac
  name="ghalet_${version}_linux_${arch}"
  tar -xzf "$archive_dir/$name.tar.gz" -C "$staging" "$name/ghalet"
  mkdir -p "$binary_dir/$arch"
  install -m 0755 "$staging/$name/ghalet" "$binary_dir/$arch/ghalet"
  export ARCH="$arch" BINARY="$binary_dir/$arch/ghalet"
  nfpm package --config packaging/nfpm.yaml --packager deb --target "$output_dir/"
  nfpm package --config packaging/nfpm.yaml --packager rpm --target "$output_dir/"
done
