#!/usr/bin/env bash
# Copyright 2026 The forgo Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# Builds the Go test plugins with forgo, packages them as one VST3 module,
# and validates it with pluginval. See README.md.
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
#   VST3_SDK_DIR      local VST3 SDK checkout (default: fetched by CMake)

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"

case "$(uname -s)" in
Darwin) os=macOS ext=.dylib exe= ;;
Linux) os=Linux ext=.so exe= ;;
MINGW* | MSYS* | CYGWIN*) os=Windows ext=.dll exe=.exe ;;
*) echo "run.sh: unsupported OS $(uname -s)" >&2; exit 2 ;;
esac

forgo="${FORGO:-$root/bin/forgo$exe}"
build="${BUILD_DIR:-$here/build}"
version="${PLUGINVAL_VERSION:-v1.0.4}"
strictness="${STRICTNESS:-10}"
repeat="${REPEAT:-3}"
rounds="${ROUNDS:-1}"
timeout="${TIMEOUT_MS:-300000}"

# native converts a path for the Windows tools (cmake, pluginval).
native() {
	if [ "$os" = Windows ]; then cygpath -m "$1"; else echo "$1"; fi
}

echo "=== building Go libraries with $forgo"
"$forgo" version
golibs="$build/go"
mkdir -p "$golibs"
(
	cd "$here"
	GOROOT="$(native "$(cd "$(dirname "$forgo")/.." && pwd)")"
	export GOROOT GOTOOLCHAIN=local CGO_ENABLED=1
	"$forgo" build -buildmode=c-shared -o "$(native "$golibs/forgo-gain$ext")" ./gain
	"$forgo" build -buildmode=c-shared -o "$(native "$golibs/forgo-drive$ext")" ./drive
)
# A byte-identical copy, so one Go binary is loaded twice from two paths.
cp "$golibs/forgo-gain$ext" "$golibs/forgo-gain-copy$ext"
rm -f "$golibs"/*.h

echo "=== building the VST3 module"
cmake_args=(-S "$(native "$here")" -B "$(native "$build/cmake")" -DCMAKE_BUILD_TYPE=Release
	-DGO_LIB_DIR="$(native "$golibs")")
if [ -n "${VST3_SDK_DIR:-}" ]; then
	cmake_args+=(-DVST3_SDK_DIR="$(native "$VST3_SDK_DIR")")
fi
cmake "${cmake_args[@]}"
cmake --build "$(native "$build/cmake")" --config Release --parallel
bundle="$(find "$build/cmake" -type d -name forgo-go-plugins.vst3 | head -1)"
if [ -z "$bundle" ]; then
	echo "run.sh: forgo-go-plugins.vst3 was not built" >&2
	exit 1
fi

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
			(cd "$dir" && cmake -E tar xf pluginval.zip)
		fi
	fi
	case "$os" in
	macOS) pluginval="$dir/pluginval.app/Contents/MacOS/pluginval" ;;
	*) pluginval="$dir/pluginval$exe" ;;
	esac
fi
"$pluginval" --version

# pluginval tests every class in the module in this one process, so the
# three Go runtimes end up loaded side by side.
for ((round = 1; round <= rounds; round++)); do
	echo "=== pluginval round $round/$rounds: strictness $strictness, repeat $repeat"
	log="$build/pluginval-round$round.log"
	status=0
	"$pluginval" --strictness-level "$strictness" --repeat "$repeat" --randomise \
		--skip-gui-tests --timeout-ms "$timeout" --validate "$(native "$bundle")" 2>&1 |
		tee "$log" || status=$?
	if [ "$status" -ne 0 ]; then
		echo "run.sh: pluginval failed in round $round (exit $status)" >&2
		exit 1
	fi
	tested="$(grep -c '^Testing plugin: ' "$log" || true)"
	if [ "$tested" -ne 3 ]; then
		echo "run.sh: pluginval tested $tested plugin classes, want 3" >&2
		exit 1
	fi
done
echo "PASS"
