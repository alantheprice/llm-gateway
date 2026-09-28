#!/usr/bin/env bash
# Link a GPU to __SERVER__ as "__NAME__".
# Installs llm-link-agent as a systemd *user* service (no root needed):
# the agent dials OUT to the gateway; no inbound ports are opened.
# Only this engine's model paths are relayed: __ENGINE__
set -euo pipefail

SERVER="__SERVER__"
NAME="__NAME__"
ENGINE="${ENGINE:-__ENGINE__}"   # host:port=model-id of your local OpenAI-compatible server

mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
echo "Downloading llm-link-agent..."
curl -fsSL "$SERVER/downloads/llm-link-agent-linux-amd64" -o "$HOME/.local/bin/llm-link-agent.new"
chmod +x "$HOME/.local/bin/llm-link-agent.new"
mv "$HOME/.local/bin/llm-link-agent.new" "$HOME/.local/bin/llm-link-agent"

# The token is stored only in this file (mode 0600), never on the command line.
( umask 077; printf 'LLM_LINK_TOKEN=%s\n' '__TOKEN__' > "$HOME/.config/llm-link-agent-$NAME.env" )

cat > "$HOME/.config/systemd/user/llm-link-agent-$NAME.service" <<UNIT
[Unit]
Description=llm-link-agent ($NAME -> $SERVER)
After=network-online.target

[Service]
EnvironmentFile=%h/.config/llm-link-agent-$NAME.env
ExecStart=%h/.local/bin/llm-link-agent --server $SERVER --name $NAME --engine $ENGINE
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable --now "llm-link-agent-$NAME"
sleep 2
systemctl --user --no-pager status "llm-link-agent-$NAME" | head -5 || true
if ! loginctl show-user "$USER" -p Linger 2>/dev/null | grep -q yes; then
  echo "Note: run 'sudo loginctl enable-linger $USER' so the agent keeps running after you log out."
fi
echo "Done. Check the My GPUs page on $SERVER — \"$NAME\" should show as connected."
