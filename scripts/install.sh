#!/bin/sh
# install.sh — install the tickwarden binary and its systemd unit.
#
# Idempotent and non-destructive: it copies files into place and reloads
# systemd, but never enables, starts, or removes anything. You decide when to
# start the service (see the printed next steps).
#
# Usage:
#   sudo ./scripts/install.sh [path-to-tickwarden-binary]
#
# If no binary path is given, it looks for ./tickwarden next to the repo root,
# then for one already on PATH.

set -e

BIN_DEST="/usr/local/bin/tickwarden"
UNIT_DEST="/etc/systemd/system/tickwarden-adaptive.service"

# Resolve the repo root from this script's location so it works from anywhere.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
UNIT_SRC="$REPO_ROOT/dist/systemd/tickwarden-adaptive.service"

# --- locate the binary to install -------------------------------------------
BIN_SRC="$1"
if [ -z "$BIN_SRC" ]; then
	if [ -x "$REPO_ROOT/tickwarden" ]; then
		BIN_SRC="$REPO_ROOT/tickwarden"
	elif command -v tickwarden >/dev/null 2>&1; then
		BIN_SRC=$(command -v tickwarden)
	fi
fi

if [ -z "$BIN_SRC" ] || [ ! -f "$BIN_SRC" ]; then
	echo "error: no tickwarden binary found." >&2
	echo "  build it first:  go build -o tickwarden ./cmd/tickwarden" >&2
	echo "  or pass a path:  sudo $0 /path/to/tickwarden" >&2
	exit 1
fi

if [ ! -f "$UNIT_SRC" ]; then
	echo "error: systemd unit not found at $UNIT_SRC" >&2
	exit 1
fi

# --- require root for the system paths ---------------------------------------
if [ "$(id -u)" -ne 0 ]; then
	echo "error: this script writes to $BIN_DEST and $UNIT_DEST — run with sudo." >&2
	exit 1
fi

# --- install (idempotent: install -m overwrites in place) --------------------
echo "Installing binary: $BIN_SRC -> $BIN_DEST"
install -m 0755 "$BIN_SRC" "$BIN_DEST"

echo "Installing unit:   $UNIT_SRC -> $UNIT_DEST"
install -m 0644 "$UNIT_SRC" "$UNIT_DEST"

# Reload systemd if it's present (skip gracefully on non-systemd hosts).
if command -v systemctl >/dev/null 2>&1; then
	echo "Reloading systemd unit files..."
	systemctl daemon-reload
fi

cat <<'EOF'

Installed. tickwarden is at /usr/local/bin/tickwarden and the unit is in place.

The unit ships in DRY-RUN by default (it logs decisions but changes nothing).

Next steps:
  1. Review/edit the ExecStart flags (URL, --apply, etc.):
       sudo systemctl edit --full tickwarden-adaptive
  2. Enable + start it:
       sudo systemctl enable --now tickwarden-adaptive
  3. Watch the decisions:
       journalctl -u tickwarden-adaptive -f

EOF
