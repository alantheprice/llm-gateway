# Link Agent — remote GPUs without VPNs

Extend the gateway's model pool to GPU machines on other networks. The
remote machine runs the link agent, which dials OUT to your gateway —
no inbound ports, no VPN. Each registered engine becomes a normal pool
member (scoring, failover, usage accounting all apply).

## Gateway side (hub)

1. Mint a link token for the remote machine (any gateway API key works;
   `role=link` recommended so it's identifiable). Admin → Keys, or:

   ```bash
   curl -s https://your-gw/api/keys -H "Cookie: <admin session>" \
     -d '{"action":"create_key","key_id":"link-dave"}'
   ```

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
engines. The agent:

- dials the gateway over outbound TLS only (NAT/firewall friendly)
- reconnects automatically (1s→30s backoff)
- relays ONLY the declared engine port(s) and ONLY model-serving paths
  (`/v1/*`, `/health`, `/usage`, `/slots`, `/metrics`) — everything else
  is rejected before it touches your LAN
- streams responses chunk-by-chunk (SSE safe)

Run it under systemd (`Restart=always`); a systemd template ships in
`deploy/llm-link-agent.service`.

## revoking

Delete the link token on the Keys page — the agent's socket dies within
one ping interval and its backends drop from the pool.

## Limitations

- The gateway must be reachable from the agent (public URL or shared
  private network). The agent dials out; the gateway never dials in.
- One relay hop (gateway ⇄ agent). No multi-hop mesh.
- Latency: add the network RTT between gateway and agent to TTFB.
