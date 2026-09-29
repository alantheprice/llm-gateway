# Linking a GPU

Link a GPU you run to this gateway, so you can use it from anywhere through the gateway's API, with its analytics, keys and rate limits.

**A linked GPU is private to you.** Only you can see or call it until you share it with people you name or with everyone. You administer it the way a platform admin administers the gateway's own GPUs: who may use it, whether it helps serve platform models, pausing it, and who used it.

You don't open ports, set up a VPN, or give the gateway access to your network. A small agent on your GPU machine connects **out** to the gateway, and requests reach your engine through that connection.

Do it from **My GPUs** in the gateway's menu. This guide explains each step.

## How it works

```
your app ──HTTPS──> gateway ──(the agent's own outbound connection)──> llm-link-agent ──> your engine (127.0.0.1)
```

**The agent:**
- runs as a systemd *user* service on your GPU machine (no root needed)
- dials out to the gateway over TLS, registers the engine(s) you declared, and relays requests to them
- reconnects on its own if the connection drops

**What the gateway can reach on your machine:** only the engine address you declared (for example `127.0.0.1:8000`), and on it only model-serving paths: `/v1/*`, `/health`, `/usage`, `/slots`, `/metrics`. Nothing else on your machine or network.

**Your link token:**
- can only connect an agent; it is **not an API key** and can't call models
- is shown once, inside the install script, and is stored on your machine in a `0600` file

**Revoking:** revoking the link in the UI disconnects the agent immediately.

**Privacy:** whoever runs a GPU can see the prompts sent to it. If you share your GPU, or let it serve a platform model, *you* can see other people's prompts on your machine. Treat that responsibly. The reverse holds too: when you call a GPU someone shared with you, its owner can see your prompts.

## Requirements

- **Linux** (x86-64 or ARM64) with systemd, or **macOS** (Apple Silicon or Intel). The installer detects which and sets up the right background service.
- **An OpenAI-compatible engine** running on the machine (step 1). The agent works with any server that answers `/v1/chat/completions`.
- **Outbound HTTPS** to the gateway. No inbound access is needed.

## Step 1: Prepare your engine

Start your engine bound to `127.0.0.1`, so only the agent (on the same machine) can reach it. Note its **port** and the **model id** it serves.

| Engine | Example | Default port | Model id |
|---|---|---|---|
| **NInfer** (full stats + model card) | `ninfer-serve model.ninfer --host 127.0.0.1 --port 8006 --model-id my-model --model-card card.json` | set with `--port` | `--model-id` |
| **vLLM** | `vllm serve Qwen/Qwen3-8B --host 127.0.0.1 --port 8000 --served-model-name my-model` | 8000 | `--served-model-name` |
| **llama.cpp** | `llama-server -m model.gguf --host 127.0.0.1 --port 8080 --alias my-model` | 8080 | `--alias` |
| **Ollama** (Linux, Mac) | `ollama serve` (then `ollama pull llama3.1:8b`) | 11434 | the model tag, e.g. `llama3.1:8b` |
| **MLX** (Mac, Apple Silicon) | `mlx_lm.server --model mlx-community/Qwen3-8B-4bit --host 127.0.0.1 --port 8080` | 8080 | the `--model` value |
| **LM Studio** (Linux, Mac) | Developer → Start Server, or `lms server start` | 1234 | the model identifier LM Studio shows |

llama.cpp runs on a Mac too (Metal). On a Mac, MLX is usually the fastest choice for Apple Silicon.

**Image models (text to image)** work the same way, as long as the engine speaks OpenAI's images API: `POST /v1/images/generations` with a `prompt`, answering with `data[].b64_json` or `data[].url`. Clients then call the gateway's `/v1/images/generations` with your GPU's name. Names such as FLUX, SDXL or Stable Diffusion are recognized as image models automatically. For anything else, an admin ticks **Image generation** under the model's capabilities on the **Routing** page. Generating an image can take a while on a laptop GPU; the gateway waits for it.

To confirm the model id your engine serves:

```bash
curl -s http://127.0.0.1:<port>/v1/models
```

The `id` field is the value to enter as **Model id**.

**NInfer engines get the most out of the gateway.** They report load, cache and energy, and can serve a model card (weights, quantization, hardware), which shows up as **Stats for nerds** in the chat. Other engines work for serving, with fewer metrics: llama.cpp and vLLM report their load; for Ollama, MLX and LM Studio the gateway counts the requests it has sent them.

**What the model can do** (chat, embeddings, image input, tool calls, thinking) is read from the engine where it says so: the NInfer model card, llama.cpp `/props`, Ollama's model details. It shows as badges on the **Models** page, and an admin can correct it on the **Routing** page.

## Step 2: Create the link

In **My GPUs → Link a GPU**, enter:

- **GPU name:** a short name for this machine, e.g. `office-4090` (lowercase letters, digits, `.`, `_`, `-`). You'll call the GPU as `<name>/<model-id>`, e.g. `office-4090/qwen3-8b`.
- **Engine address:** `host:port` of your engine as the agent sees it, normally `127.0.0.1:<port>`.
- **Model id:** from step 1.

Click **Create link**. You get an install script with your link token inside. **It's shown once**, so copy it now.

Regular users can hold up to 3 active links.

## Step 3: Install the agent

On the GPU machine:

```bash
bash link-<name>.sh
```

**The script:**
1. downloads the right `llm-link-agent` build for the machine (Linux or macOS, x86-64 or ARM64) into `~/.local/bin`
2. stores the token in `~/.config/llm-link-agent-<name>.token` (mode 0600)
3. starts the agent in the background:
   - **Linux:** the systemd user service `llm-link-agent-<name>`
   - **Mac:** a LaunchAgent (`~/Library/LaunchAgents/com.llm-gateway.link-agent.<name>.plist`) that starts at login and restarts if it stops

**Keep it running:**
- **Linux**, after you log out: `sudo loginctl enable-linger $USER`
- **Mac:** the agent runs while you're logged in. Stop the Mac from sleeping when you want it to serve (System Settings → Energy, or `caffeinate -s` while plugged in).

## Step 4: Verify

**My GPUs** shows the link as **connected** within a few seconds. After the first metrics poll (up to ~10 s) the engine shows its lanes, and, for NInfer engines with a model card, a summary like `nvfp4 weights · KV nvfp4 · dflash2 ×7 · RTX 5090 · 240K ctx`.

**Check the agent's log on the GPU machine:**

```bash
journalctl --user -u llm-link-agent-<name> -f      # Linux
tail -f ~/Library/Logs/llm-link-agent-<name>.log    # Mac
```

A healthy start looks like:

```
connected to https://<gateway>
registered: [http://link/<name>:<port>]
agent: relay GET /slots
```

## Step 5: Use it and share it

**Call it.** Your GPU is listed in `GET /v1/models` (for you) and in the chat's model picker as `<gpu-name>/<model-id>`:

```bash
curl $GATEWAY/v1/chat/completions -H "Authorization: Bearer $YOUR_API_KEY" \
  -d '{"model": "office-4090/qwen3-8b", "messages": [{"role": "user", "content": "hi"}]}'
```

You also get the admin views for it: `GET /v1/models/<gpu-name>/<model-id>` returns the full model card, and `/details` returns live host and GPU facts. Other people get the redacted card only.

**Share it.** On **My GPUs**, choose who can call the GPU by its name:

| Setting | Who can call `<gpu-name>/<model-id>` |
|---|---|
| **Only me** (default) | You |
| **People I name** | You and the usernames you add |
| **Everyone** | Every signed-in user on the gateway |

People you share with:
- see the model in their `/v1/models` and chat picker, and under **My GPUs → Shared with me**
- call it with their own API key; the requests count against their own quota
- lose access at once when you remove them or switch back to **Only me**

Requests without an API key (anonymous LAN access) never reach a linked GPU.

**While your GPU serves a platform model** (below), other people reach it only through that model, which balances load across its GPUs. Its own name stays yours for testing, whatever the sharing setting. The setting applies again once it stops serving platform models.

The link's **Usage** table shows who used it over the last 7 days: requests, tokens, latency and errors.

## Platform models

A platform model is a shared name such as `qwen3.8-27b`, which the gateway routes across several GPUs by load, cache affinity and context window. Everyone with an API key can call it.

A GPU helps serve a platform model **only when both sides agree**:
1. An **admin** adds the GPU to that shared model on the **Routing** page (it appears in the GPU list as soon as its agent connects).
2. **You** tick that model on the link in **My GPUs**.

Until you tick it, no platform traffic reaches your GPU, even if an admin has listed it. Ticking it means anyone's prompts for that model may be routed to your machine. Untick it, or pause the link, to stop at once.

GPUs linked by admins are platform infrastructure: they serve the models that list them without the extra step. **My GPUs** shows this as "no opt-in is needed" on each of those models.

Once serving, the GPU's requests appear in **Analytics → Per service** (one row per engine), and NInfer model cards appear in `GET /v1/models/<platform-model>` and in **Stats for nerds**.

## Managing a link

| Status | Meaning | What to do |
|---|---|---|
| **connected** | The agent is online and registered | Nothing |
| **not connected** | The token exists, but no agent is connected with it | Run the install script, or check the agent's log (step 4) |
| **revoked** | The token was revoked; the agent can't connect | Create a new link if you still want this GPU |
| **paused** | You paused it; the agent stays connected, and nobody is routed to it | **Resume** |
| engine **waiting for first poll** | Just connected | Wait ~10 s |
| engine **down** | The agent is connected, but the engine isn't answering | Check the engine is running on the declared address |

- **Pause / Resume:** stops all routing to the GPU (yours, shared users', platform models) without touching the machine.
- **Revoke:** the agent disconnects at once and can't reconnect with that token. Sharing and platform-model settings are removed.
- **Replace a token:** create a new link (a new name, or revoke the old one first), run its script, then revoke the old link.
- **Several engines on one machine:** create one link per engine, or add more `--engine host:port=model-id` flags to the service (`systemctl --user edit --full llm-link-agent-<name>`).

## Troubleshooting

| Agent log / symptom | Cause | Fix |
|---|---|---|
| `gateway refused the link token (401)` | The link was revoked, or the token is wrong | Create a new link and run its script |
| `gateway refused registration: agent version … is too old` | The agent binary is outdated | Re-run the install script; it downloads the current agent |
| `gateway refused registration: agent name "…" is registered to another user` | Someone else's agent holds that name | Create a link with a different name |
| `link down: … — reconnecting in …` repeating | Network or gateway unreachable | Check outbound HTTPS to the gateway; the agent keeps retrying (1 s → 30 s) |
| Connected, but the engine is **down** | The engine isn't listening on the declared address | Start the engine; confirm with `curl http://127.0.0.1:<port>/v1/models` |
| Stops after logout (Linux) | User services end at logout without lingering | `sudo loginctl enable-linger $USER` |
| Stops when the Mac sleeps | The Mac is asleep, so the agent can't answer | Prevent sleep while serving (System Settings → Energy, or `caffeinate -s`) |
| Windows | Not supported by the installer | Use Linux (WSL may work, untested) or a Mac |

## Uninstall

**Linux:**

```bash
systemctl --user disable --now llm-link-agent-<name>
rm ~/.config/systemd/user/llm-link-agent-<name>.service ~/.config/llm-link-agent-<name>.token
systemctl --user daemon-reload
```

**Mac:**

```bash
launchctl bootout gui/$(id -u)/com.llm-gateway.link-agent.<name>
rm ~/Library/LaunchAgents/com.llm-gateway.link-agent.<name>.plist ~/.config/llm-link-agent-<name>.token
```

Then revoke the link in **My GPUs**.
