#!/usr/bin/env sh
# Installs forgo (https://github.com/lsegal/forgo) on Linux or macOS.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/lsegal/forgo/release-branch.go1.26/install/install.sh | sh -s -- v0.2.0
#
# Env vars:
#   FORGO_REPO         "owner/repo" to install from (default: lsegal/forgo)
#   FORGO_INSTALL_DIR  install directory (default: $HOME/.forgo)
set -eu

REPO="${FORGO_REPO:-lsegal/forgo}"
INSTALL_DIR="${FORGO_INSTALL_DIR:-$HOME/.forgo}"
VERSION="${1:-latest}"

os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
Linux) goos="linux" ;;
Darwin) goos="darwin" ;;
*)
	echo "error: unsupported OS: $os (forgo supports Linux and macOS here; see install/install.ps1 for Windows)" >&2
	exit 1
	;;
esac

case "$arch" in
x86_64 | amd64) goarch="amd64" ;;
arm64 | aarch64) goarch="arm64" ;;
*)
	echo "error: unsupported architecture: $arch" >&2
	exit 1
	;;
esac

asset="forgo-${goos}-${goarch}.tar.gz"

if [ "$VERSION" = "latest" ]; then
	url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
	case "$VERSION" in
	v*) tag="$VERSION" ;;
	*) tag="v${VERSION}" ;;
	esac
	url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
fi

echo "Installing forgo (${VERSION}) for ${goos}/${goarch}..."
echo "  from: ${url}"
echo "  to:   ${INSTALL_DIR}"

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q "$1" -O "$2"; }
else
	echo "error: forgo's installer needs curl or wget" >&2
	exit 1
fi

tmpfile="$(mktemp)"
tmpdir="$(mktemp -d)"
cleanup() { rm -rf "$tmpfile" "$tmpdir"; }
trap cleanup EXIT

if ! fetch "$url" "$tmpfile"; then
	echo "error: failed to download ${url}" >&2
	echo "       check that '${VERSION}' is a real forgo release for ${goos}/${goarch}." >&2
	exit 1
fi

tar -xzf "$tmpfile" -C "$tmpdir" --strip-components=1

rm -rf "$INSTALL_DIR"
mkdir -p "$(dirname "$INSTALL_DIR")"
mv "$tmpdir" "$INSTALL_DIR"

installed_version="(unknown)"
[ -f "$INSTALL_DIR/FORGO_VERSION" ] && installed_version="$(cat "$INSTALL_DIR/FORGO_VERSION")"

echo ""
echo "forgo ${installed_version} installed to ${INSTALL_DIR}"

marker_start="# >>> forgo installer >>>"
marker_end="# <<< forgo installer <<<"

posix_block="export GOROOT=\"${INSTALL_DIR}\"
export PATH=\"${INSTALL_DIR}/bin:\$PATH\""

fish_block="set -gx GOROOT \"${INSTALL_DIR}\"
set -gx PATH \"${INSTALL_DIR}/bin\" \$PATH"

add_block() {
	file="$1"
	block="$2"
	if [ -f "$file" ] && grep -qF "$marker_start" "$file" 2>/dev/null; then
		# Already installed once; refresh in place in case INSTALL_DIR changed.
		awk -v s="$marker_start" -v e="$marker_end" '
			$0 == s {skip=1}
			!skip {print}
			$0 == e {skip=0}
		' "$file" >"$file.forgo.tmp" && mv "$file.forgo.tmp" "$file"
	fi
	{
		echo ""
		echo "$marker_start"
		printf '%s\n' "$block"
		echo "$marker_end"
	} >>"$file"
}

# bash, ash, and plain sh all read one of these depending on invocation.
for rc in "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile" "$HOME/.zshrc"; do
	add_block "$rc" "$posix_block"
done

if command -v fish >/dev/null 2>&1 || [ -d "$HOME/.config/fish" ]; then
	mkdir -p "$HOME/.config/fish"
	add_block "$HOME/.config/fish/config.fish" "$fish_block"
fi

echo ""
echo "Added to your shell startup files (.bashrc, .bash_profile, .profile, .zshrc, and fish config if present):"
echo ""
echo "$posix_block"
echo ""
echo "Then verify with:"
echo "  forgo version"
echo ""

# Resync the current shell's environment by replacing it with a fresh login
# shell that re-reads the startup files we just updated.
if [ -n "${SHELL:-}" ]; then
	echo "Restarting your shell (${SHELL}) to pick up the new PATH..."
	exec "$SHELL" -l
fi
