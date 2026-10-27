#!/usr/bin/env bash
# Package the built plugin for distribution.
#
#   ./package.sh              # build then package
#   SKIP_BUILD=1 ./package.sh # package what is already in dist/plugins
set -euo pipefail

cd "$(dirname "$0")"

NAME=raccoon2api
VERSION=$(grep -E '^\s*pluginVersion\s*=' schema.go | head -1 | sed -E 's/.*"([^"]+)".*/\1/')
VERSION=${VERSION:-0.0.0}
STAGE="dist/stage/${NAME}-${VERSION}"

if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
  ./build.sh
fi

rm -rf "$STAGE"
mkdir -p "$STAGE"

find dist/plugins -type f \( -name "*.so" -o -name "*.dll" -o -name "*.dylib" \) | while read -r artifact; do
  relative=${artifact#dist/plugins/}
  mkdir -p "$STAGE/plugins/$(dirname "$relative")"
  cp "$artifact" "$STAGE/plugins/$relative"
done

cp README.md config.example.yaml "$STAGE/"
[[ -f LICENSE ]] && cp LICENSE "$STAGE/"

(
  cd "$STAGE"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
)

mkdir -p dist
rm -f "dist/${NAME}-${VERSION}.zip"
(
  cd dist/stage
  if command -v zip >/dev/null 2>&1; then
    zip -qr "../${NAME}-${VERSION}.zip" "${NAME}-${VERSION}"
  elif command -v python >/dev/null 2>&1; then
    python -m zipfile -c "../${NAME}-${VERSION}.zip" "${NAME}-${VERSION}"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -m zipfile -c "../${NAME}-${VERSION}.zip" "${NAME}-${VERSION}"
  else
    MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd):/w" -w /w alpine:3 sh -c "apk add --no-cache zip >/dev/null && zip -qr ../${NAME}-${VERSION}.zip ${NAME}-${VERSION}"
  fi
)

echo "package: dist/${NAME}-${VERSION}.zip"
