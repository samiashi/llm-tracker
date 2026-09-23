#!/bin/sh
# Install the llm-tracker agent on a Mac.
#
#   gh api repos/samiashi/llm-tracker/contents/install.sh \
#     -H "Accept: application/vnd.github.raw" | sh
#
# Releases are private, so this goes through `gh`: a plain download of a
# private asset returns a login page, which is worse than an error because it
# installs. `gh auth login` is the only prerequisite.
#
# Nothing is asked. The agent enrols itself with the team's tracker as gh's
# account: the server checks that account is in the GitHub org and issues this
# machine an upload token of its own.
set -eu

REPO="samiashi/llm-tracker"
ORG="${REPO%%/*}"
BIN_DIR="$HOME/.local/bin"
NAME="llm-tracker-agent"

die() { printf '%s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

[ "$(uname -s)" = "Darwin" ] ||
  die "This agent runs under launchd and is macOS only (found $(uname -s))."

command -v gh >/dev/null 2>&1 ||
  die "The GitHub CLI is required for a private release.
  brew install gh && gh auth login"

# gh honours GH_HOST from the environment, so anything that set it would
# redirect the download while REPO made the source look fixed.
export GH_HOST=github.com

# Only the host the download uses: plain `gh auth status` fails when any other
# host or account has a stale token.
gh auth status --hostname github.com >/dev/null 2>&1 ||
  die "gh is not signed in to github.com. Run: gh auth login"

# Named up front: gh may hold a personal account as well as a work one, and
# only one in $ORG can download the release or enrol.
LOGIN="$(gh api user --jq .login 2>/dev/null)" ||
  die "gh could not read its GitHub account. Run: gh auth status"
say "Installing as $LOGIN."

# uname -m says x86_64 in a shell running under Rosetta, which would install
# the Intel build on Apple silicon, where hw.optional.arm64 is 1 regardless.
# An Intel Mac has no such key, which -i turns into an empty answer.
if [ "$(sysctl -in hw.optional.arm64)" = 1 ]; then
  ARCH=arm64
elif [ "$(uname -m)" = x86_64 ]; then
  ARCH=amd64
else
  die "No release is built for $(uname -m)."
fi
ASSET="$NAME-darwin-$ARCH"

TMP="$(mktemp -d)"
cleanup() {
  _rc=$?
  rm -rf "$TMP"
  exit "$_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

say "Downloading $ASSET..."
gh release download --repo "$REPO" --pattern "$ASSET" \
  --output "$TMP/$NAME" --clobber ||
  die "Could not download $ASSET from $REPO as $LOGIN. Either no release is
  published yet, or $LOGIN cannot see the repository: sign gh in as your
  $ORG account (gh auth login, or gh auth switch if it is already added)."

# Verified before it is ever executed.
gh release download --repo "$REPO" --pattern "$ASSET.sha256" \
  --output "$TMP/sum" --clobber ||
  die "No published checksum for $ASSET; refusing to install an unverified binary."

EXPECTED="$(cut -d' ' -f1 < "$TMP/sum")"
ACTUAL="$(shasum -a 256 "$TMP/$NAME" | cut -d' ' -f1)"
[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] ||
  die "Checksum mismatch for $ASSET.
  published $EXPECTED
  received  $ACTUAL"

chmod 755 "$TMP/$NAME"
# gh writes no quarantine attribute, but a hand-placed download might.
xattr -d com.apple.quarantine "$TMP/$NAME" 2>/dev/null || true

# A wrong-architecture asset would otherwise become a collector that fails
# silently every five minutes.
NEW_VERSION="$("$TMP/$NAME" version 2>/dev/null)" ||
  die "The downloaded binary does not run on this machine."

mkdir -p "$BIN_DIR" && [ -w "$BIN_DIR" ] || die "Cannot write to $BIN_DIR."

# Enrolled and installed from the temp copy, and moved onto PATH only once
# both succeed: enroll saves nothing the server has not accepted, and no
# failure or Ctrl-C before the move touches a CLI already in BIN_DIR.
"$TMP/$NAME" enroll || die "Not installed: fix the problem above and re-run."

# install copies the binary to ~/.llm-tracker/bin and registers the
# LaunchAgent, so the collector does not depend on BIN_DIR.
"$TMP/$NAME" install || die "Not installed: fix the problem above and re-run."

mv -f "$TMP/$NAME" "$BIN_DIR/$NAME"
say "Installed $NEW_VERSION to $BIN_DIR/$NAME"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say ""
     say "  $BIN_DIR is not on your PATH. Add it:"
     say "    echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc" ;;
esac

say ""
say "Done. The collector runs at login and every 5 minutes."
say "  $NAME status     what it has found and when it last synced"
say "  $NAME upgrade    fetch the current release"
