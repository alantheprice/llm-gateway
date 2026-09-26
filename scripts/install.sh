#!/usr/bin/env bash
# llm-gateway installer (Linux + systemd).
#
#   curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install.sh | bash
#
# Flags (after --):
#   --version vX.Y.Z   pin a release (default: latest)
#   --build            build from source instead of downloading (needs Go 1.27+)
#   --port N           gateway port (default 8033)
#   --uninstall        remove service (keep data; add --purge to delete it)
#   --purge            with --uninstall: also delete /opt/llm-gateway + /var/lib/llm-gateway
set -euo pipefail

REPO="sprout-foundry/llm-gateway"
DL_BASE="https://github.com/${REPO}/releases/download"
GW_USER="llmgateway"
OPT_DIR="/opt/llm-gateway"
DATA_DIR="/var/lib/llm-gateway"
SERVICE="llm-gateway"
GW_PORT="8033"
VERSION=""
BUILD=0
UNINSTALL=0
PURGE=0

# ---------- arg parsing (works when piped: read from /dev/tty or args) ----
ARGS=()
if [ $# -eq 0 ] && [ -t 0 ]; then :; fi
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --build) BUILD=1; shift ;;
    --port) GW_PORT="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    *) echo "unknown flag: $1"; exit 1 ;;
  esac
done

SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  SUDO="sudo"
fi

uninstall() {
  $SUDO systemctl disable --now "${SERVICE}.service" 2>/dev/null || true
  $SUDO rm -f "/etc/systemd/system/${SERVICE}.service"
  $SUDO systemctl daemon-reload
  if [ "$PURGE" = "1" ]; then
    $SUDO rm -rf "$OPT_DIR" "$DATA_DIR"
    echo "Purged $OPT_DIR and $DATA_DIR."
  else
    echo "Service removed. Data kept in $DATA_DIR (use --purge to delete)."
  fi
  echo "llm-gateway uninstalled."
  exit 0
}

[ "$UNINSTALL" = "1" ] && uninstall

# ---------- helpers -------------------------------------------------------
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

need() { command -v "$1" >/dev/null 2>&1; }

arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) echo "unsupported arch: $(uname -m) (want x86_64 or aarch64)" >&2; exit 1 ;;
  esac
}

# ---------- resolve version ----------------------------------------------
if [ "$BUILD" = "0" ]; then
  if [ -z "$VERSION" ]; then
    say "Resolving latest release"
    if need curl; then
      VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | cut -d'"' -f4)
    elif need wget; then
      VERSION=$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | cut -d'"' -f4)
    fi
    [ -n "$VERSION" ] || { echo "could not resolve latest release; pass --version vX.Y.Z"; exit 1; }
  fi
  say "Installing llm-gateway ${VERSION}"
fi

# ---------- user + dirs ---------------------------------------------------
say "Creating user + directories"
id "$GW_USER" >/dev/null 2>&1 || $SUDO useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$GW_USER"
$SUDO mkdir -p "$OPT_DIR" "$DATA_DIR/pb"

# ---------- binary --------------------------------------------------------
if [ "$BUILD" = "1" ]; then
  say "Building from source"
  need git || { echo "git required for --build"; exit 1; }
  need go || { echo "go (1.27+) required for --build"; exit 1; }
  TMP=$(mktemp -d)
  git clone --depth 1 "https://github.com/${REPO}.git" "$TMP/src"
  (cd "$TMP/src" \
    && (cd internal/server && DOCS_DIR=../../docs OUT_DIR=guide go run generate_guide.go) \
    && go build -ldflags "-s -w -X main.version=$(git -C "$TMP/src" describe --tags --always)" -o "$TMP/llm-gateway" ./cmd/gateway)
  $SUDO install -m755 "$TMP/llm-gateway" "$OPT_DIR/llm-gateway"
  rm -rf "$TMP"
else
  A=$(arch)
  URL="${DL_BASE}/${VERSION}/llm-gateway-${VERSION}-linux-${A}.tar.gz"
  say "Downloading $URL"
  TMP=$(mktemp -d)
  if need curl; then curl -fsSL "$URL" -o "$TMP/pkg.tgz"; else wget -qO "$TMP/pkg.tgz" "$URL"; fi
  tar xzf "$TMP/pkg.tgz" -C "$TMP"
  $SUDO install -m755 "$TMP/llm-gateway-linux-${A}" "$OPT_DIR/llm-gateway"
  rm -rf "$TMP"
fi

# ---------- config (only if absent) --------------------------------------
if [ ! -f "$OPT_DIR/llm_gateway.conf" ]; then
  say "Writing default config"
  $SUDO tee "$OPT_DIR/llm_gateway.conf" >/dev/null <<'CONF'
{
  "gateway": {"port": PORT_PLACEHOLDER, "trust_local_networks": true, "models_require_auth": false},
  "local_networks": ["192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"],
  "public_models": ["*"],
  "model_pools": {},
  "overflow_pairs": {},
  "discovery": {"local_ports": [8000,8001,8002,8003,8004,8005,8006,8007,8008,8009,8010]},
  "metrics": {"poll_interval": 10, "stale_threshold": 30, "default_max_seqs": 3},
  "cache": {"ttl": 60}
}
CONF
  $SUDO sed -i "s/PORT_PLACEHOLDER/${GW_PORT}/" "$OPT_DIR/llm_gateway.conf"
fi

# ---------- systemd unit --------------------------------------------------
say "Installing ${SERVICE}.service"
$SUDO tee "/etc/systemd/system/${SERVICE}.service" >/dev/null <<UNIT
[Unit]
Description=llm-gateway - LLM routing, identity, cost accounting
After=network.target

[Service]
Type=simple
User=${GW_USER}
WorkingDirectory=${OPT_DIR}
ExecStart=${OPT_DIR}/llm-gateway
Restart=always
RestartSec=3
Environment=LLM_GATEWAY_CONF=${OPT_DIR}/llm_gateway.conf
Environment=PB_DATA_DIR=${DATA_DIR}/pb

NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${OPT_DIR} ${DATA_DIR}
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
$SUDO systemctl daemon-reload

# ---------- ownership ----------------------------------------------------
$SUDO chown -R "$GW_USER:$GW_USER" "$OPT_DIR" "$DATA_DIR"

# ---------- boot + bootstrap admin --------------------------------------
say "Starting ${SERVICE}"
$SUDO systemctl enable --now "${SERVICE}"
sleep 3
for i in $(seq 1 10); do
  curl -fsS "http://127.0.0.1:${GW_PORT}/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -fsS "http://127.0.0.1:${GW_PORT}/health" >/dev/null || {
  echo "gateway did not become healthy — check: journalctl -u ${SERVICE} -n 30"; exit 1;
}

say "Provisioning first admin"
ADMIN_USER="admin"
ADMIN_PASS=$($SUDO -u "$GW_USER" env LLM_GATEWAY_CONF="$OPT_DIR/llm_gateway.conf" \
  PB_DATA_DIR="$DATA_DIR/pb" "$OPT_DIR/llm-gateway" user create-admin "$ADMIN_USER" 2>/dev/null \
  | grep 'Password (shown once)' | awk '{print $NF}')

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
IP=${IP:-<host-ip>}

cat <<EOF

============================================================
 llm-gateway ${VERSION:-} installed and running

  URL:       http://${IP}:${GW_PORT}/
  Login:     admin
  Password:  ${ADMIN_PASS}   (shown ONCE — log in and change it)

  Guide:     http://${IP}:${GW_PORT}/guide
  Next step: add an engine (see /guide/ninfer-engine) or point
             model_pools at an existing OpenAI-compatible server,
             then mint a key on the Keys page.
============================================================
EOF
