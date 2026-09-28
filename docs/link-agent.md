# Link Agent — remote GPUs without VPNs

Extend the gateway's model pool to GPU machines on other networks. The
remote machine runs the link agent, which dials OUT to your gateway —
no inbound ports, no VPN. Each registered engine becomes a normal pool
member (scoring, failover, usage accounting all apply).

## Gateway side (hub)

1. Mint a link token for the remote machine. A link can only be opened
   by a key with `role=link` or a key owned by an **admin** — every
   prompt routed to a link reaches that machine, so ordinary user keys
   are refused. Admin → Keys (as an admin), or:

   ```bash
   curl -s https://your-gw/api/keys -H "Cookie: <admin session>" \
     -d '{"action":"create_key","key_id":"link-dave"}'
   ```

   The key's user **owns** the agent name it first registers (e.g.
   `dave-gpu`); an agent using another user's key cannot take that name
   over while the gateway runs.

2. Add the remote engine to a pool once the agent connects — members
   reference the virtual URL `http://link/<agent-name>:<engine-port>`:

   ```json
   "model_pools": {"qwen3-32b": {"members": [
     {"model_id": "qwen3-32b", "backend": "http://link/dave-gpu:8006"}
   ]}}
   ```

## Remote machine (agent)

```bash
llm-link-agent \
  --server https://your-gw.example.com \
  --token sk-link-... \
  --name dave-gpu \
  --engine 127.0.0.1:8006=qwen3-32b:4
```

`--engine host:port=model-id[:max-concurrency]`, repeatable for several
engines. If the local engine requires its own API key, pass
`--engine-key` (or `LLM_LINK_ENGINE_KEY`). Agent names are lowercased and
limited to `a-z 0-9 . _ -`. The agent:

- dials the gateway over outbound TLS only (NAT/firewall friendly)
- reconnects automatically (1s→30s backoff)
- relays ONLY the declared engine port(s) and ONLY model-serving paths
  (`/v1/*`, `/health`, `/usage`, `/slots`, `/metrics`) — everything else
  is rejected before it touches your LAN
- streams responses chunk-by-chunk (SSE safe), many requests at once
- never sees your users' gateway credentials: the gateway relays only
  `Content-Type` and `Accept`, and the agent drops any `Authorization` /
  `Cookie` it receives (it sends its own `--engine-key`, if set)
- aborts the engine request when the client disconnects (cancel frame)

Run it under systemd (`Restart=always`); a systemd template ships in
`deploy/llm-link-agent.service`.

## Failure behaviour

- The gateway treats an agent socket that is silent for 60 s as dead
  (agents ping every 15 s); every in-flight request on it fails and the
  link's backends drop out of the pool.
- A response nobody reads (client gone, failover loser) is cut off and
  cancelled at the agent; it never stalls other requests on the link.

## revoking

Delete the link token on the Keys page — the agent's socket dies within
one ping interval and its backends drop from the pool.

## Limitations

- The gateway must be reachable from the agent (public URL or shared
  private network). The agent dials out; the gateway never dials in.
- One relay hop (gateway ⇄ agent). No multi-hop mesh.
- Latency: add the network RTT between gateway and agent to TTFB.
