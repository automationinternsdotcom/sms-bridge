#!/bin/bash
# Install (or reload) the always-on LaunchAgent for the current user.
set -euo pipefail
LABEL="${SMSBRIDGE_LABEL:-com.smsbridge.daemon}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/smsbridge"
DATA="$HOME/Library/Application Support/sms-bridge"
DST="$HOME/Library/LaunchAgents/$LABEL.plist"
[ -x "$BIN" ] || { echo "build first: go build -o bin/smsbridge ./cmd/smsbridge" >&2; exit 1; }
mkdir -p "$HOME/Library/LaunchAgents" "$DATA"
chmod 700 "$DATA"
esc() { printf '%s' "$1" | sed -e 's/[&|\\]/\\&/g'; }
sed -e "s|__LABEL__|$(esc "$LABEL")|" -e "s|__BIN__|$(esc "$BIN")|" -e "s|__DATA__|$(esc "$DATA")|g" \
  "$ROOT/launchd/smsbridge.plist.template" > "$DST"
plutil -lint "$DST" >/dev/null
if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  for _ in $(seq 1 30); do launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || break; sleep 1; done
fi
launchctl bootstrap "gui/$(id -u)" "$DST"
launchctl print "gui/$(id -u)/$LABEL" | grep -E "^\s*state =" | head -1
