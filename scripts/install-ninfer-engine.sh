#!/usr/bin/env bash
# NInfer engine installer — builds the gateway's telemetry fork
# (branch nvfp4-upstream-master of alantheprice/ninfer-4090) and installs
# it as a systemd-served engine.
#
#   curl -fsSL .../scripts/install-ninfer-engine.sh | bash -s -- --arch 120a
#
# Flags:
#   --arch 120a|90a   CUDA arch (120a = RTX 5090/6000 Pro, 90a = 4090-class)
#   --model-id NAME   --model-id for the engine (default qwen3.8-27b)
#   --port N          engine port (default 8006)
#   --electricity R   $/kWh for energy accounting (default 0.125)
#   --kv-capacity N   KV tokens (default 600000; raise on 96GB cards)
#   --concurrency N   max streams (default 4; 8 fits 96GB cards)
set -euo pipefail

ARCH="120a"
MODEL_ID="qwen3.8-27b"
PORT="8006"
RATE="0.125"
KV="600000"
CONC="4"
BRANCH="nvfp4-upstream-master"
REPO_URL="https://github.com/alantheprice/ninfer-4090.git"
INSTALL_DIR="/opt/ninfer"
STATE_DIR="/var/lib/ninfer"

while [ $# -gt 0 ]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --model-id) MODEL_ID="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --electricity) RATE="$2"; shift 2 ;;
    --kv-capacity) KV="$2"; shift 2 ;;
    --concurrency) CONC="$2"; shift 2 ;;
    *) echo "unknown flag: $1"; exit 1 ;;
  esac
done

SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1; }

for t in git cmake ninja; do
  need "$t" || { echo "missing: $t (required)"; exit 1; }
done
nvcc --version >/dev/null 2>&1 || { echo "nvcc not found — install CUDA toolkit 13.x first"; exit 1; }

say "Cloning the fork (${BRANCH})"
[ -d "$INSTALL_DIR/.git" ] || $SUDO git clone -b "$BRANCH" "$REPO_URL" "$INSTALL_DIR"
cd "$INSTALL_DIR"
$SUDO git fetch origin "$BRANCH" 2>/dev/null || true

say "Building (CUDA arch ${ARCH})"
$SUDO env PATH=/usr/local/cuda-13.1/bin:/usr/local/cuda/bin:$PATH \
  cmake -S . -B build -G Ninja -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_CUDA_ARCHITECTURES="$ARCH"
$SUDO env PATH=/usr/local/cuda-13.1/bin:/usr/local/cuda/bin:$PATH \
  ninja -C build -j"$(nproc)"
$SUDO install -m755 build/apps/ninfer-serve /usr/local/bin/ninfer-serve

say "Model artifact"
if ls "$INSTALL_DIR"/models/*.ninfer >/dev/null 2>&1; then
  echo "Found artifact(s) in $INSTALL_DIR/models/ — using the newest."
  ARTIFACT=$(ls -t "$INSTALL_DIR"/models/*.ninfer | head -1)
else
  ARTIFACT="$STATE_DIR/model.ninfer"
  echo "No *.ninfer found in $INSTALL_DIR/models/. Place your v3 artifact at:"
  echo "  $ARTIFACT"
  echo "(or copy from an existing box; conversion recipe in the gateway docs)"
fi

say "Model placement + state dir"
$SUDO mkdir -p "$STATE_DIR"
[ -f "$ARTIFACT" ] && [ "$ARTIFACT" != "$STATE_DIR/model.ninfer" ] && \
  $SUDO cp "$ARTIFACT" "$STATE_DIR/model.ninfer" || true

ID=$($SUDO systemd-id128 new 2>/dev/null | head -1 || echo engine)
need getent && ! getent passwd ninfer >/dev/null && $SUDO useradd --system --home "$STATE_DIR" --shell /usr/sbin/nologin ninfer 2>/dev/null || true
$SUDO chown -R ninfer:ninfer "$STATE_DIR" 2>/dev/null || true

say "Installing ninfer-engine.service"
$SUDO tee /etc/systemd/system/ninfer-engine.service >/dev/null <<UNIT
[Unit]
Description=NInfer serve - llm-gateway engine (${MODEL_ID})
After=network.target

[Service]
Type=simple
User=ninfer
ExecStart=/usr/local/bin/ninfer-serve ${STATE_DIR}/model.ninfer \\
  --host 0.0.0.0 --port ${PORT} \\
  --model-id ${MODEL_ID} \\
  --max-concurrency ${CONC} \\
  --kv-capacity ${KV} \\
  --electricity-rate ${RATE} \\
  --metrics-state ${STATE_DIR}/metrics-state.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
$SUDO systemctl daemon-reload

cat <<EOF

============================================================
 ninfer-serve installed.

 Next steps:
  1. Put a v3 model artifact at ${STATE_DIR}/model.ninfer
     (if not already), then:
       sudo systemctl enable --now ninfer-engine
  2. Watch the startup log's free-VRAM line and tune
     --kv-capacity / --max-concurrency accordingly.
  3. Point the gateway pool at http://<this-box>:${PORT}
     (model id: ${MODEL_ID}).
============================================================
EOF
