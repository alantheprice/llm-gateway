#!/bin/sh
# llm-gateway client setup — stores your API key, adds it to your shell
# profile, and registers the gateway as a sprout provider (if sprout is
# installed or installable).
#
# Usage:  curl -fsSL <gateway>/static/setup-key.sh | sh -s -- <your-key>
# Re-run any time to update the key. Safe to re-run.
set -eu

KEY="${1:-}"
if [ -z "$KEY" ]; then
  echo "usage: setup-key.sh <api-key>" >&2
  echo "  (get one from the llm-gateway Keys page)" >&2
  exit 1
fi

# ---------- locate the gateway URL (the host this script was served from)
# The gateway rewrites this placeholder at serve time.
BASE_URL="__GATEWAY_URL__"
PROVIDER_NAME="__PROVIDER_NAME__"
DEFAULT_MODEL="__DEFAULT_MODEL__"

# ---------- shell profile detection --------------------------------------
SHELL_NAME=$(basename "${SHELL:-/bin/sh}")
case "$SHELL_NAME" in
  zsh)  PROFILE="${ZDOTDIR:-$HOME}/.zshrc" ;;
  bash) PROFILE="${HOME}/.bashrc" ;;
  *)    PROFILE="${HOME}/.profile" ;;
esac

ENV_LINE="export LLM_GATEWAY_API_KEY=$KEY"
ENV_BASE="export LLM_GATEWAY_URL=$BASE_URL"

touch "$PROFILE"
if grep -q "LLM_GATEWAY_API_KEY" "$PROFILE" 2>/dev/null; then
  # replace existing block lines
  sed -i.bak-setupkey \
    -e "/^export LLM_GATEWAY_API_KEY=/c\\$ENV_LINE" \
    -e "/^export LLM_GATEWAY_URL=/c\\$ENV_BASE" \
    "$PROFILE"
  rm -f "$PROFILE.bak-setupkey"
else
  printf '\n# llm-gateway (added by setup-key.sh)\n%s\n%s\n' "$ENV_LINE" "$ENV_BASE" >> "$PROFILE"
fi
echo "✓ key stored in $PROFILE"

# ---------- sprout provider (optional) ------------------------------------
if command -v sprout >/dev/null 2>&1; then
  PROV_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/sprout/providers"
  mkdir -p "$PROV_DIR"
  cat > "$PROV_DIR/$PROVIDER_NAME.json" <<JSON
{
  "name": "$PROVIDER_NAME",
  "endpoint": "$BASE_URL/v1",
  "model_name": "$DEFAULT_MODEL",
  "requires_api_key": true,
  "env_var": "LLM_GATEWAY_API_KEY",
  "include_usage": true
}
JSON
  chmod 600 "$PROV_DIR/$PROVIDER_NAME.json"
  echo "✓ sprout provider '$PROVIDER_NAME' registered (endpoint $BASE_URL/v1, model $DEFAULT_MODEL)"
  echo "  use it:  sprout config set provider $PROVIDER_NAME"
else
  echo "ℹ sprout not found — install it to use this gateway as a coding provider:"
  echo "    curl -fsSL https://raw.githubusercontent.com/sprout-foundry/sprout/main/scripts/install.sh | sh"
  echo "  then re-run this script to register the provider."
fi

echo
echo "Done. Start a new shell (or: source $PROFILE) and test:"
echo "  curl -s \$LLM_GATEWAY_URL/v1/chat/completions -H \"Authorization: Bearer \$LLM_GATEWAY_API_KEY\" \\"
echo "    -d '{\"model\":\"$DEFAULT_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
