#!/usr/bin/env bash
# Builds the mobile bindings with gomobile. Not run in CI.
#   mobile/build.sh android|ios|all     (default: all)
# Tags: the nano tag set from profiles.txt; override with TAGS="..." (space separated).
set -euo pipefail
cd "$(dirname "$0")/.."

target="${1:-all}"
tags="${TAGS:-$(awk '$1=="nano" {for (i=3;i<=NF;i++) printf "%s ", $i}' profiles.txt)}"
tags="${tags% }"
mkdir -p out

need() { # name, install hint
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "error: '$1' not found." >&2
  echo "$2" >&2
  exit 1
}

need go "Install Go from https://go.dev/dl/"
if ! command -v gomobile >/dev/null 2>&1; then
  cat >&2 <<'MSG'
error: 'gomobile' not found. Install it once:
  go install golang.org/x/mobile/cmd/gomobile@latest
  go install golang.org/x/mobile/cmd/gobind@latest
  gomobile init
and make sure $(go env GOPATH)/bin is on PATH.
MSG
  exit 1
fi

build_android() {
  if [ -z "${ANDROID_NDK_HOME:-}" ] && [ -z "${ANDROID_HOME:-}" ] && [ -z "${ANDROID_SDK_ROOT:-}" ]; then
    echo "error: Android SDK/NDK not found. Install Android Studio, then the NDK (SDK Manager > SDK Tools > NDK)" >&2
    echo "       and set ANDROID_HOME (and ANDROID_NDK_HOME if the NDK is not under \$ANDROID_HOME/ndk)." >&2
    exit 1
  fi
  echo "==> android (tags: $tags)"
  gomobile bind -target android -androidapi 24 -trimpath -ldflags "-s -w" -tags "$tags" -o out/tokibase.aar ./mobile
  ls -l out/tokibase.aar
}

build_ios() {
  if [ "$(uname)" != "Darwin" ] || ! command -v xcodebuild >/dev/null 2>&1; then
    echo "error: the iOS build needs macOS with Xcode (xcodebuild) and the command line tools (xcode-select --install)." >&2
    exit 1
  fi
  echo "==> ios (tags: $tags)"
  gomobile bind -target ios -trimpath -ldflags "-s -w" -tags "$tags" -o out/TokiBase.xcframework ./mobile
  du -sh out/TokiBase.xcframework
}

case "$target" in
  android) build_android ;;
  ios) build_ios ;;
  all) build_android; build_ios ;;
  *) echo "usage: $0 [android|ios|all]" >&2; exit 2 ;;
esac
