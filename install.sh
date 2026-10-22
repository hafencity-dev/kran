#!/bin/sh
# kran installer: downloads the release for this machine into ~/.local/bin and
# runs `kran setup`. No sudo. Safe to run again (it upgrades in place).
#
# The repository hafencity-dev/kran is private, so the download needs either
# the GitHub CLI (`gh auth login`) or a token in GITHUB_TOKEN / GH_TOKEN.
#
#   gh api repos/hafencity-dev/kran/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh
#   gh api repos/hafencity-dev/kran/contents/install.sh -H 'Accept: application/vnd.github.raw' | sh -s -- --version 0.3.0
#
# Options (or environment): --version X (VERSION), --no-setup (KRAN_NO_SETUP=1),
# KRAN_INSTALL_DIR (default ~/.local/bin).
set -eu

REPO="hafencity-dev/kran"
VERSION="${VERSION:-}"
INSTALL_DIR="${KRAN_INSTALL_DIR:-$HOME/.local/bin}"
NO_SETUP="${KRAN_NO_SETUP:-0}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value, e.g. --version 0.3.0"; VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#--version=}"; shift ;;
	--no-setup) NO_SETUP=1; shift ;;
	-h | --help)
		say "usage: install.sh [--version X] [--no-setup]   (env: VERSION, KRAN_INSTALL_DIR, KRAN_NO_SETUP, GITHUB_TOKEN)"
		exit 0 ;;
	*) die "unknown option $1 (try --help)" ;;
	esac
done
VERSION="${VERSION#v}"

case "$(uname -s)" in
Linux) OS=linux ;;
Darwin) OS=darwin ;;
*) die "unsupported OS $(uname -s); kran supports Linux and macOS" ;;
esac
case "$(uname -m)" in
aarch64 | arm64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*) die "unsupported CPU $(uname -m); kran supports arm64 and amd64" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

have() { command -v "$1" >/dev/null 2>&1; }

download_gh() {
	if [ -n "$VERSION" ]; then set -- "v$VERSION"; else set --; fi
	gh release download "$@" -R "$REPO" -D "$TMP" \
		-p "kran_*_${OS}_${ARCH}.tar.gz" -p checksums.txt >/dev/null
}

# gh_curl passes the token in a curl config on stdin (printf is a builtin),
# never in argv, where other users on the machine could read it in ps.
gh_curl() {
	printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | curl -K - "$@"
}

download_token() {
	have curl || die "curl is required to download with a token"
	api="https://api.github.com/repos/$REPO/releases"
	if [ -n "$VERSION" ]; then api="$api/tags/v$VERSION"; else api="$api/latest"; fi
	gh_curl -fsSL -H "Accept: application/vnd.github+json" "$api" -o "$TMP/release.json" ||
		die "could not read the release ($api); check the version and that your token can read $REPO"
	for pattern in "kran_[^\"]*_${OS}_${ARCH}.tar.gz" "checksums.txt"; do
		# In the API JSON an asset's "url" precedes its "name".
		asset="$(awk -v pat="^\"?($pattern)\"?\$" '
			/"url": "https:\/\/api\.github\.com\/repos\/[^"]*\/releases\/assets\/[0-9]+"/ { u = $2; gsub(/[",]/, "", u) }
			/^[ \t]*"name": "/ { n = $2; gsub(/[",]/, "", n); if (u != "" && n ~ pat) { print n " " u; exit } }
		' "$TMP/release.json")"
		[ -n "$asset" ] || die "release has no asset matching $pattern for $OS/$ARCH"
		gh_curl -fsSL -H "Accept: application/octet-stream" \
			"${asset#* }" -o "$TMP/${asset%% *}" || die "download of ${asset%% *} failed"
	done
}

TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
if have gh && gh auth status >/dev/null 2>&1; then
	say "Downloading kran for $OS/$ARCH with the GitHub CLI..."
	download_gh || die "gh could not download the release; check the version and your access to $REPO"
elif [ -n "$TOKEN" ]; then
	say "Downloading kran for $OS/$ARCH with your token..."
	download_token
else
	die "$REPO is private, so I need GitHub access. Either run 'gh auth login' (https://cli.github.com) and re-run this script, or set GITHUB_TOKEN to a token that can read $REPO."
fi

ARCHIVE="$(find "$TMP" -maxdepth 1 -name "kran_*_${OS}_${ARCH}.tar.gz" | head -n 1)"
ARCHIVE="${ARCHIVE##*/}"
[ -n "$ARCHIVE" ] || die "download finished but no kran archive for $OS/$ARCH was found"

# Verify against the published checksums.
[ -f "$TMP/checksums.txt" ] || die "checksums.txt is missing from the release; refusing to install an unverified binary"
want="$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no entry for $ARCHIVE"
if have sha256sum; then got="$(sha256sum "$TMP/$ARCHIVE" | awk '{print $1}')"
elif have shasum; then got="$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{print $1}')"
else die "need sha256sum or shasum to verify the download"; fi
[ "$got" = "$want" ] || die "checksum mismatch for $ARCHIVE (expected $want, got $got); try again"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" kran || die "could not unpack $ARCHIVE"
mkdir -p "$INSTALL_DIR"
chmod 755 "$TMP/kran"
# Copy next to the target first so the final mv is atomic (a running kran keeps working).
cp "$TMP/kran" "$INSTALL_DIR/.kran.new.$$"
mv -f "$INSTALL_DIR/.kran.new.$$" "$INSTALL_DIR/kran"
if [ "$OS" = darwin ]; then xattr -d com.apple.quarantine "$INSTALL_DIR/kran" 2>/dev/null || true; fi

say "Installed $("$INSTALL_DIR/kran" version 2>/dev/null | head -n 1 || echo kran) to $INSTALL_DIR/kran"

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	case "$(basename "${SHELL:-sh}")" in
	fish) line="fish_add_path $INSTALL_DIR" ;;
	*) line="export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
	esac
	# shellcheck disable=SC2088 # shown to the user, not expanded
	case "$(basename "${SHELL:-sh}")" in
	zsh) rc="~/.zshrc" ;;
	bash) rc="~/.bashrc" ;;
	fish) rc="~/.config/fish/config.fish" ;;
	*) rc="your shell profile" ;;
	esac
	say "$INSTALL_DIR is not on your PATH. Add this line to $rc and open a new terminal:"
	say "  $line"
	;;
esac

if [ "$NO_SETUP" = 1 ]; then
	say "Skipping setup. Run 'kran setup' when you are ready."
	exit 0
fi
if [ -t 0 ]; then
	"$INSTALL_DIR/kran" setup
else
	"$INSTALL_DIR/kran" setup --yes
fi
