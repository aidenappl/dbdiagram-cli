#!/usr/bin/env sh
# Cross-compile dbdiagram for darwin/linux/windows × amd64/arm64 into dist/.
# Local build only: nothing is uploaded, tagged or published.
#   VERSION=v0.1.0 scripts/release.sh
set -eu
cd "$(dirname "$0")/.."
# Release archives carry LICENSE, NOTICE and THIRD_PARTY_NOTICES; refuse to
# ship notices that no longer match the module graph being built.
fresh="$(mktemp)"
scripts/third_party_notices.sh "$fresh" >/dev/null
if ! cmp -s "$fresh" THIRD_PARTY_NOTICES; then
	rm -f "$fresh"
	echo "THIRD_PARTY_NOTICES is stale: run scripts/third_party_notices.sh and commit it" >&2
	exit 1
fi
rm -f "$fresh"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
rm -rf dist
mkdir -p dist
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	out="dist/dbdiagram_${VERSION}_${os}_${arch}"
	bin="dbdiagram"
	[ "$os" = windows ] && bin="dbdiagram.exe"
	mkdir -p "$out"
	cp LICENSE NOTICE THIRD_PARTY_NOTICES "$out/"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
		-ldflags="-w -s -X main.version=${VERSION}" -o "$out/$bin" .
	echo "built $out/$bin"
done
(cd dist && for d in */; do d="${d%/}"; tar -czf "$d.tar.gz" -C "$d" .; done && shasum -a 256 ./*.tar.gz > SHA256SUMS)
echo "dist/ ready (version ${VERSION})"
