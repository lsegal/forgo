#!/usr/bin/env bash
# Copyright 2026 The forgo Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# Builds the Go VST3 modules with forgo, lays out their .vst3 bundles, and
# validates each with pluginval. See README.md.
#
# Environment:
#   FORGO             forgo binary (default: bin/forgo of this checkout);
#                     its tree is used as GOROOT
#   PLUGINVAL         pluginval binary (default: download PLUGINVAL_VERSION)
#   PLUGINVAL_VERSION pluginval release to download (default: v1.0.4)
#   STRICTNESS        pluginval strictness level (default: 10)
#   REPEAT            pluginval --repeat count (default: 3)
#   ROUNDS            how many times to run pluginval (default: 1)
#   TIMEOUT_MS        fail when pluginval prints nothing for this long
#                     (default: 300000)
#   BUILD_DIR         build output (default: pluginval/build)

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"

case "$(uname -m)" in
x86_64 | amd64) arch=x86_64 ;;
arm64 | aarch64) arch=arm64 ;;
*) echo "run.sh: unsupported architecture $(uname -m)" >&2; exit 2 ;;
esac
# bindir is the directory in a .vst3 bundle that holds its binary.
case "$(uname -s)" in
Darwin) os=macOS ext=.dylib exe= bindir=MacOS ;;
Linux)
	os=Linux ext=.so exe=
	if [ "$arch" = arm64 ]; then bindir=aarch64-linux; else bindir=x86_64-linux; fi
	;;
MINGW* | MSYS* | CYGWIN*) os=Windows ext=.dll exe=.exe bindir=$arch-win ;;
*) echo "run.sh: unsupported OS $(uname -s)" >&2; exit 2 ;;
esac

forgo="${FORGO:-$root/bin/forgo$exe}"
build="${BUILD_DIR:-$here/build}"
version="${PLUGINVAL_VERSION:-v1.0.4}"
strictness="${STRICTNESS:-10}"
repeat="${REPEAT:-3}"
rounds="${ROUNDS:-1}"
timeout="${TIMEOUT_MS:-300000}"

# native converts a path for the Windows tools (forgo, pluginval).
native() {
	if [ "$os" = Windows ]; then cygpath -m "$1"; else echo "$1"; fi
}

echo "=== building Go modules with $forgo"
"$forgo" version
golibs="$build/go"
rm -rf "$golibs"
mkdir -p "$golibs"
(
	cd "$here"
	GOROOT="$(native "$(cd "$(dirname "$forgo")/.." && pwd)")"
	export GOROOT GOTOOLCHAIN=local CGO_ENABLED=1
	"$forgo" test ./...
	for m in gain drive loader; do
		"$forgo" build -buildmode=c-shared -o "$(native "$golibs/forgo-$m$ext")" "./$m"
	done
)
rm -f "$golibs"/*.h

# bundle NAME BINARY [LIBRARY...] lays out NAME.vst3 around BINARY, with
# each LIBRARY next to it.
bundle() {
	local name="$1" bin="$2" dir="$build/$1.vst3"
	shift 2
	rm -rf "$dir"
	mkdir -p "$dir/Contents/$bindir"
	case "$os" in
	macOS)
		cp "$bin" "$dir/Contents/MacOS/$name"
		cat >"$dir/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key><string>English</string>
	<key>CFBundleExecutable</key><string>$name</string>
	<key>CFBundleIdentifier</key><string>dev.forgo.pluginval.$name</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleName</key><string>$name</string>
	<key>CFBundlePackageType</key><string>BNDL</string>
	<key>CFBundleSignature</key><string>????</string>
	<key>CFBundleVersion</key><string>1.0.0</string>
</dict>
</plist>
PLIST
		;;
	Windows) cp "$bin" "$dir/Contents/$bindir/$name.vst3" ;;
	*) cp "$bin" "$dir/Contents/$bindir/$name.so" ;;
	esac
	if [ $# -gt 0 ]; then cp "$@" "$dir/Contents/$bindir/"; fi
}

echo "=== laying out the VST3 bundles"
# Each Go plugin on its own.
bundle forgo-go-gain "$golibs/forgo-gain$ext"
bundle forgo-go-drive "$golibs/forgo-drive$ext"
# The loader with every plugin, including a byte-identical copy of gain,
# so one Go binary is loaded twice from two paths.
cp "$golibs/forgo-gain$ext" "$golibs/forgo-gain-copy$ext"
bundle forgo-go-plugins "$golibs/forgo-loader$ext" \
	"$golibs/forgo-gain$ext" "$golibs/forgo-drive$ext" "$golibs/forgo-gain-copy$ext"

pluginval="${PLUGINVAL:-}"
if [ -z "$pluginval" ]; then
	dir="$build/pluginval-$version"
	if [ ! -d "$dir" ]; then
		echo "=== downloading pluginval $version"
		mkdir -p "$dir"
		curl -fsSL -o "$dir/pluginval.zip" \
			"https://github.com/Tracktion/pluginval/releases/download/$version/pluginval_$os.zip"
		if command -v unzip >/dev/null; then
			(cd "$dir" && unzip -q pluginval.zip)
		else
			powershell -NoProfile -Command "Expand-Archive -Path '$(native "$dir/pluginval.zip")' -DestinationPath '$(native "$dir")'"
		fi
	fi
	case "$os" in
	macOS) pluginval="$dir/pluginval.app/Contents/MacOS/pluginval" ;;
	*) pluginval="$dir/pluginval$exe" ;;
	esac
fi
"$pluginval" --version

# validate MODULE CLASSES runs pluginval on MODULE.vst3 and checks that it
# tested CLASSES plugin classes.
validate() {
	local module="$1" want="$2" log="$build/pluginval-$1-round$round.log" status=0 tested
	echo "=== pluginval round $round/$rounds: $module, strictness $strictness, repeat $repeat"
	"$pluginval" --strictness-level "$strictness" --repeat "$repeat" --randomise \
		--skip-gui-tests --timeout-ms "$timeout" --validate "$(native "$build/$module.vst3")" 2>&1 |
		tee "$log" || status=$?
	if [ "$status" -ne 0 ]; then
		echo "run.sh: pluginval failed for $module in round $round (exit $status)" >&2
		exit 1
	fi
	tested="$(grep -c '^Testing plugin: ' "$log" || true)"
	if [ "$tested" -ne "$want" ]; then
		echo "run.sh: pluginval tested $tested plugin classes in $module, want $want" >&2
		exit 1
	fi
}

# pluginval tests every class in a module in one process, so the loader's
# run has four Go runtimes loaded side by side: the loader, gain, drive,
# and the copy of gain.
for ((round = 1; round <= rounds; round++)); do
	validate forgo-go-gain 1
	validate forgo-go-drive 1
	validate forgo-go-plugins 3
done
echo "PASS"
