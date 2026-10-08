#!/usr/bin/env bash
# Builds the mobile bindings with gomobile. Not run in CI.
#   mobile/build.sh android|ios|all     (default: all)
# Tags: the nano tag set from profiles.txt; override with TAGS="..." (space separated).
# ANDROID_TARGETS: gomobile target list, default "android" (all four ABIs);
#   e.g. ANDROID_TARGETS=android/arm64,android/amd64 for a smaller/faster build.
# Works on macOS and Linux (see "Android AAR on Linux" in docs/EMBED.md).
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
  sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
  if [ -z "${ANDROID_NDK_HOME:-}" ] && [ -n "$sdk" ] && [ -d "$sdk/ndk" ]; then
    # newest NDK under the SDK root when ANDROID_NDK_HOME is not set
    ANDROID_NDK_HOME="$sdk/ndk/$(ls "$sdk/ndk" | sort -V | tail -1)"
    export ANDROID_NDK_HOME
  fi
  if [ -z "${ANDROID_NDK_HOME:-}" ] || [ ! -d "$ANDROID_NDK_HOME" ]; then
    echo "error: Android NDK not found. Install it (Android Studio SDK Manager, or" >&2
    echo "       sdkmanager --sdk_root=\$ANDROID_HOME 'ndk;27.2.12479018') and set ANDROID_HOME" >&2
    echo "       (and ANDROID_NDK_HOME if the NDK is not under \$ANDROID_HOME/ndk)." >&2
    exit 1
  fi
  # gomobile needs the SDK platform (android.jar) and a JDK (javac) to compile the Java glue.
  need javac "Install a JDK 17+ (for example Temurin) and put it on PATH / set JAVA_HOME."
  echo "==> android (tags: $tags, targets: ${ANDROID_TARGETS:-android}, ndk: $ANDROID_NDK_HOME)"
  # -mod=mod lets the go command add golang.org/x/mobile/bind (needed by the generated bindings)
  # to go.mod/go.sum for this build; those two files are restored afterwards.
  cp go.mod out/go.mod.bak; cp go.sum out/go.sum.bak
  trap 'cp out/go.mod.bak go.mod; cp out/go.sum.bak go.sum' EXIT
  GOFLAGS="${GOFLAGS:--mod=mod}" gomobile bind -target "${ANDROID_TARGETS:-android}" -androidapi 24 -trimpath -ldflags "-s -w" -tags "$tags" -o out/tokibase.aar ./mobile
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
