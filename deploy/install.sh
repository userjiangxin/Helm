#!/usr/bin/env bash
# Install helmd on this machine and start it as a user service.
#   deploy/install.sh --server https://helm.<tailnet>.ts.net --code <registration code> [--labels gpu,dingtalk] [--binary ./bin/helmd]
set -euo pipefail

SERVER="" CODE="" LABELS="" BINARY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --code) CODE="$2"; shift 2 ;;
    --labels) LABELS="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    *) echo "unknown flag $1" >&2; exit 2 ;;
  esac
done
[[ -n "$SERVER" && -n "$CODE" ]] || { echo "usage: $0 --server <url> --code <code> [--labels a,b] [--binary path]" >&2; exit 2; }

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(dirname "$HERE")"
if [[ -z "$BINARY" ]]; then
  echo "building helmd from source…"
  (cd "$REPO/daemon" && CGO_ENABLED=0 go build -o "$REPO/bin/helmd" ./cmd/helmd)
  BINARY="$REPO/bin/helmd"
fi

mkdir -p "$HOME/.local/bin" "$HOME/.helm"
chmod 700 "$HOME/.helm"
install -m 0755 "$BINARY" "$HOME/.local/bin/helmd"
"$HOME/.local/bin/helmd" register --server "$SERVER" --code "$CODE"
if [[ -n "$LABELS" ]]; then
  echo "HELM_LABELS=$LABELS" > "$HOME/.helm/env"
  chmod 600 "$HOME/.helm/env"
fi

case "$(uname -s)" in
  Linux)
    mkdir -p "$HOME/.config/systemd/user"
    cp "$HERE/systemd/helmd.service" "$HOME/.config/systemd/user/helmd.service"
    systemctl --user daemon-reload
    systemctl --user enable --now helmd
    loginctl enable-linger "$USER" 2>/dev/null || echo "note: run 'sudo loginctl enable-linger $USER' so helmd survives logout"
    echo "helmd running. logs: journalctl --user -u helmd -f" ;;
  Darwin)
    PLIST="$HOME/Library/LaunchAgents/com.helm.helmd.plist"
    mkdir -p "$(dirname "$PLIST")"
    sed "s#__HOME__#$HOME#g" "$HERE/launchd/com.helm.helmd.plist" > "$PLIST"
    if [[ -n "$LABELS" ]]; then
      /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:HELM_LABELS string $LABELS" "$PLIST"
    fi
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load -w "$PLIST"
    echo "helmd running. logs: tail -f ~/.helm/helmd.log" ;;
  *) echo "unsupported OS; run '$HOME/.local/bin/helmd run' yourself" ;;
esac
