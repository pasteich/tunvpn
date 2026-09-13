#!/usr/bin/env bash
# Build the whole project: Android tunnel binary (CGO/NDK), the tun2socks
# gomobile AAR, and the debug APK.
#
# Requirements (set these or edit below):
#   ANDROID_HOME      -> Android SDK (with platform-34, build-tools;34.0.0)
#   ANDROID_NDK_HOME  -> NDK r26d (…/ndk/26.3.11579264)
#   JDK 17, Gradle 8.7, Go 1.23+, gomobile on PATH
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
: "${ANDROID_NDK_HOME:=$ANDROID_HOME/ndk/26.3.11579264}"
ABI="${ABI:-arm64-v8a}"
GOARCH_MAP_arm64_v8a="arm64"

case "$ABI" in
  arm64-v8a) GOARCH=arm64; CCPREFIX=aarch64-linux-android24 ;;
  armeabi-v7a) GOARCH=arm; CCPREFIX=armv7a-linux-androideabi24 ;;
  x86_64) GOARCH=amd64; CCPREFIX=x86_64-linux-android24 ;;
  *) echo "unknown ABI $ABI"; exit 1 ;;
esac

CC="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/${CCPREFIX}-clang"

echo "==> 0  server binaries (embedded in app) for linux amd64/arm64"
mkdir -p "$ROOT/phone/app/src/main/assets/server"
( cd "$ROOT"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "phone/app/src/main/assets/server/notes-mail-tunnel-amd64" .
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "phone/app/src/main/assets/server/notes-mail-tunnel-arm64" . )

echo "==> 1/3  tunnel binary (libtun.so) for $ABI  [CGO on]"
mkdir -p "$ROOT/phone/app/src/main/jniLibs/$ABI"
( cd "$ROOT"
  CGO_ENABLED=1 GOOS=android GOARCH="$GOARCH" CC="$CC" \
    go build -trimpath -ldflags "-s -w" \
    -o "phone/app/src/main/jniLibs/$ABI/libtun.so" . )

echo "==> 2/3  tun2socks gomobile AAR"
( cd "$ROOT/phone/t2smobile"
  GOFLAGS=-mod=mod gomobile bind -target="android/$GOARCH" -androidapi 24 \
    -ldflags="-s -w" -o "$ROOT/phone/app/libs/t2smobile.aar" . )

echo "==> 3/3  debug APK"
( cd "$ROOT/phone"
  gradle :app:assembleDebug --no-daemon --console=plain )

echo
echo "APK: $ROOT/phone/app/build/outputs/apk/debug/app-debug.apk"
