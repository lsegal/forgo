#!/usr/bin/env bash
# forgo version bump mechanics.
#
# Usage:
#   scripts/version.sh show            # print the current version
#   scripts/version.sh major|minor|patch  # bump and print the new version
#   scripts/version.sh set X.Y.Z       # set an explicit version
#
# The version is tracked in FORGO_VERSION at the repo root (plain "X.Y.Z",
# no "v" prefix — that's added when forming a git tag / release name). This
# is forgo's own version, independent of the golang/go release it's synced
# against (see .github/workflows/upstream-sync.yml).
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

VERSION_FILE="FORGO_VERSION"

usage() {
	echo "usage: $0 {show|major|minor|patch|set <version>}" >&2
	exit 1
}

current() {
	if [ -f "$VERSION_FILE" ]; then
		tr -d '[:space:]' <"$VERSION_FILE"
	else
		echo "0.0.0"
	fi
}

[ $# -ge 1 ] || usage
cmd="$1"

ver="$(current)"
if ! [[ "$ver" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
	echo "error: $VERSION_FILE contains an invalid version: $ver" >&2
	exit 1
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

case "$cmd" in
show)
	echo "$ver"
	exit 0
	;;
major)
	major=$((major + 1))
	minor=0
	patch=0
	;;
minor)
	minor=$((minor + 1))
	patch=0
	;;
patch)
	patch=$((patch + 1))
	;;
set)
	[ $# -ge 2 ] || usage
	new="${2#v}"
	if ! [[ "$new" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "error: invalid version '$2' (expected X.Y.Z or vX.Y.Z)" >&2
		exit 1
	fi
	echo "$new" >"$VERSION_FILE"
	echo "$new"
	exit 0
	;;
*)
	usage
	;;
esac

new="$major.$minor.$patch"
echo "$new" >"$VERSION_FILE"
echo "$new"
