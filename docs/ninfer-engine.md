# NInfer Engine Runbook

The gateway routes fine against any OpenAI-compatible backend, but
**routing intelligence and energy accounting need engine telemetry**. The
NInfer fork supplies it. This runbook installs the fork as a
systemd-served engine the gateway can pool.

## What the fork adds

Three endpoints stock engines don't have (branch `nvfp4-upstream-master`
of [`alantheprice/ninfer-4090`](https://github.com/alantheprice/ninfer-4090)):

| Endpoint | Gateway feature it feeds |
|---|---|
| `GET /slots` | lane capacity + running/waiting → load score (0.75·lanes + 0.15·queue + 0.10·KV-pressure) |
| `GET /usage` | tok/s, cache-hit %, KV spills/evictions, **energy kWh + $** (NVML, `--electricity-rate`) → admin Costs page, price book |
| `GET /metrics` | Prometheus text (vLLM-compatible parser) |

Counters persist across engine restarts via `--metrics-state` — daily/30-day
energy rollups survive.

## Prerequisites

| Component | Version | Notes |
|---|---|---|
| NVIDIA GPU | sm_90 (4090/6000 Pro) or sm_120 (5090) | driver 570+ |
| CUDA toolkit | 13.x | `nvcc --version` |
| CMake | ≥ 3.28 | hard requirement |
| Ninja, GCC 13+ | any recent | C++20 |
| Disk | ~60 GB | model artifact + build |

## Install (scripted)

```bash
curl -fsSL https://raw.githubusercontent.com/sprout-foundry/llm-gateway/main/scripts/install-ninfer-engine.sh | bash -s -- --arch 120a
```

`--arch`: `120a` for RTX 5090 / 6000 Pro Ada, `90a` for 4090-class. The
script clones the fork, builds `ninfer-serve`, installs a systemd unit
(`ninfer-engine.service`), and prints the flags to confirm.

## Install (manual)

```bash
git clone -b nvfp4-upstream-master https://github.com/alantheprice/ninfer-4090.git ninfer
cd ninfer
PATH=/usr/local/cuda-13.1/bin:$PATH cmake -S . -B build -G Ninja \
      -DCMAKE_BUILD_TYPE=Release -DCMAKE_CUDA_ARCHITECTURES=120a
PATH=/usr/local/cuda-13.1/bin:$PATH ninja -C build -j$(nproc)
sudo install -m755 build/apps/ninfer-serve /usr/local/bin/
```

### Model artifact

Either copy an existing `*.ninfer` v3 artifact to the box, or convert:

```bash
huggingface-cli download Ostfralla/Qwen3.8-27B-NVFP4-NInfer \
  qwen3_8_27b_nvfp4.ninfer --local-dir models/
python3 tools/upgrade_ninfer_v2_to_v3.py \
  models/qwen3_8_27b_nvfp4.ninfer models/qwen3_8_27b_nvfp4_v3.ninfer
```

### Serve (systemd)

```ini
# /etc/systemd/system/ninfer-engine.service
[Unit]
Description=NInfer serve (llm-gateway engine)
After=network.target

[Service]
Type=simple
User=ninfer
ExecStart=/usr/local/bin/ninfer-serve /var/lib/ninfer/qwen3_8_27b_nvfp4_v3.ninfer \
  --host 0.0.0.0 --port 8006 \
  --model-id qwen3.8-27b \
  --max-context 262144 --kv-dtype fp8 \
  --max-concurrency 8 --max-pending-requests 64 \
  --spec mtp --draft-tokens 4 --lm-head-draft \
  --preserve-thinking \
  --electricity-rate 0.125 \
  --metrics-state /var/lib/ninfer/metrics-state.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

**VRAM guidance**: start conservative and read the startup log's free-VRAM
line. 96 GB cards take `--kv-capacity 1700000` at C=8; 32 GB cards should
start near `--kv-capacity 600000 --max-concurrency 4` and raise it until
the log says otherwise.

## Point the gateway at it

Add the member to the pool (`/admin/config/page` → pools, or conf):

```json
"model_pools": { "qwen3.8-27b": { "members": [
  {"model_id": "qwen3.8-27b", "backend": "http://<engine-host>:8006",
   "large_context": true, "capacity_weight": 8}
], "cache_affinity": true } }
```

`sudo systemctl restart llm-gateway`, then verify at `/backends`:

- `engine=ninfer`, `lanes=8`, `load_score` moving with load
- energy columns on `/admin/costs` start filling (needs `--electricity-rate`)

## Expected performance (validated)

| Card | Solo decode | Batched (C=8) | MTP-4 acceptance | Energy |
|---|---|---|---|---|
| RTX 6000 Pro 96GB | ~245 tok/s | 2,250–2,750 tok/s | ~77% | ~0.9 kWh / 1M out tok |
| RTX 5090 32GB | ~200–320 tok/s | C=6: 650–950 tok/s | ~77% | similar per-token |

## Engine without the fork?

vLLM works with partial signal (its `/metrics` Prometheus endpoint);
llama.cpp and stock engines work with zero load signal — routing still
happens, but scores stay 0.0 and overflow never triggers. Pin sessions and
size your pools by hand in that case.
