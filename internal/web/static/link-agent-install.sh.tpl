#!/usr/bin/env bash
# Link a GPU to __SERVER__ as "__NAME__".
# Installs llm-link-agent as a per-user background service (no root needed):
#   Linux: a systemd user service      macOS: a launchd LaunchAgent
# The agent dials OUT to the gateway; no inbound ports are opened.
# Only this engine's model paths are relayed: __ENGINE__
set -euo pipefail

SERVER="__SERVER__"
NAME="__NAME__"
ENGINE="${ENGINE:-__ENGINE__}"   # host:port=model-id of your local OpenAI-compatible server

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "Unsupported OS $(uname -s): the agent runs on Linux and macOS." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "Unsupported CPU $(uname -m): the agent runs on x86-64 and ARM64." >&2; exit 1 ;;
esac

BIN="$HOME/.local/bin/llm-link-agent"
TOKEN_FILE="$HOME/.config/llm-link-agent-$NAME.token"
mkdir -p "$HOME/.local/bin" "$HOME/.config"

echo "Downloading llm-link-agent ($OS-$ARCH)..."
curl -fsSL "$SERVER/downloads/llm-link-agent-$OS-$ARCH" -o "$BIN.new"
chmod +x "$BIN.new"
mv "$BIN.new" "$BIN"

# The token is stored only in this file (mode 0600), never on a command line
# or in the service definition.
( umask 077; printf 'LLM_LINK_TOKEN=%s\n' '__TOKEN__' > "$TOKEN_FILE" )

if [ "$OS" = linux ]; then
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/llm-link-agent-$NAME.service" <<UNIT
[Unit]
Description=llm-link-agent ($NAME -> $SERVER)
After=network-online.target

[Service]
ExecStart=%h/.local/bin/llm-link-agent --server $SERVER --name $NAME --engine $ENGINE --token-file %h/.config/llm-link-agent-$NAME.token
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable "llm-link-agent-$NAME"
  systemctl --user restart "llm-link-agent-$NAME"
  sleep 2
  systemctl --user --no-pager status "llm-link-agent-$NAME" | head -5 || true
  if ! loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q yes; then
    echo "Note: run 'sudo loginctl enable-linger $USER' so the agent keeps running after you log out."
  fi
  echo "Logs: journalctl --user -u llm-link-agent-$NAME -f"
else
  LABEL="com.llm-gateway.link-agent.$NAME"
  PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
  LOG="$HOME/Library/Logs/llm-link-agent-$NAME.log"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN</string>
    <string>--server</string><string>$SERVER</string>
    <string>--name</string><string>$NAME</string>
    <string>--engine</string><string>$ENGINE</string>
    <string>--token-file</string><string>$TOKEN_FILE</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
PLIST
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  sleep 2
  tail -n 5 "$LOG" 2>/dev/null || true
  echo "Logs: tail -f $LOG"
  echo "Note: the agent runs while you're logged in. To keep serving with the screen off,"
  echo "      stop the Mac sleeping (System Settings > Energy, or run 'caffeinate -s' while plugged in)."
fi
echo "Done. Check the My GPUs page on $SERVER — \"$NAME\" should show as connected."
