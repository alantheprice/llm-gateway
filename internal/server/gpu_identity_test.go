package server

import (
	"testing"

	"llmgateway/internal/config"
	"llmgateway/internal/embeddedpb"
)

var identHosts = []config.HostCfg{
	{Label: "gpu-b (6000 Pro)", IPs: []string{"192.168.1.200"}, Links: []string{"gpu-b"}},
	{Label: "ai-worker (5090)", IPs: []string{"127.0.0.1"}},
}

// The same engine reached directly and over its link is one GPU; another
// engine on the same box is a different GPU; unclaimed backends keep a
// readable label.
func TestIdentifyGPU(t *testing.T) {
	direct := identifyGPU(identHosts, "http://192.168.1.200:8006")
	viaLink := identifyGPU(identHosts, "http://link/gpu-b:8006")
	if direct.Key != viaLink.Key || direct.Label != "gpu-b (6000 Pro) :8006" {
		t.Fatalf("direct %+v vs link %+v: want one identity", direct, viaLink)
	}
	if direct.Via != "direct" || viaLink.Via != "link" {
		t.Fatalf("via: %q / %q", direct.Via, viaLink.Via)
	}
	fim := identifyGPU(identHosts, "http://192.168.1.200:8009")
	if fim.Key == direct.Key || fim.Host != "gpu-b (6000 Pro)" {
		t.Fatalf("FIM engine on the same box must be its own GPU: %+v", fim)
	}
	stray := identifyGPU(identHosts, "http://link/dave-gpu:8006")
	if stray.Host != "" || stray.Label != "link dave-gpu:8006" || stray.Key != "http://link/dave-gpu:8006" {
		t.Fatalf("unclaimed link = %+v", stray)
	}
}

// Engine counters for one GPU under two URLs merge by max (same engine),
// and the row reports the link as its current path.
func TestMergeGPURowsByIdentity(t *testing.T) {
	rows := []embeddedpb.GPUDailyRow{
		{Backend: "http://192.168.1.200:8006", Tokens: 100, Kwh: 2.0, CacheHits: 50, EngineInput: 80},
		{Backend: "http://link/gpu-b:8006", Tokens: 120, Kwh: 2.5, CacheHits: 40, EngineInput: 90},
		{Backend: "http://127.0.0.1:8000", Tokens: 7, Kwh: 1.0},
	}
	out := mergeGPURowsByIdentity(identHosts, rows)
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2 GPUs", len(out))
	}
	w := out[0]
	if w["tokens"] != int64(120) || w["kwh"] != 2.5 || w["cache_hits"] != int64(50) || w["via"] != "link" {
		t.Fatalf("merged 6000 Pro row = %v", w)
	}
}

// Per-request analytics rows for one GPU under two URLs sum; rates are
// recomputed from the sums.
func TestMergePerGPU(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0},"hosts":[{"label":"gpu-b (6000 Pro)","ips":["192.168.1.200"],"links":["gpu-b"]}]}`, nil)
	out := s.mergePerGPU([]embeddedpb.GPURow{
		{Backend: "http://192.168.1.200:8006", Requests: 10, Input: 1000, Cached: 500, TokPerSec: 100, DraftN: 100, DraftAccepted: 40},
		{Backend: "http://link/gpu-b:8006", Requests: 30, Input: 3000, Cached: 2700, TokPerSec: 200, DraftN: 300, DraftAccepted: 180},
	})
	if len(out) != 1 {
		t.Fatalf("rows = %d, want 1", len(out))
	}
	r := out[0]
	if r["requests"] != int64(40) || r["cache_hit_pct"] != 80.0 || r["tok_per_s"] != 175.0 ||
		r["draft_accept_pct"] != 55.0 || r["via"] != "link" {
		t.Fatalf("merged = %v", r)
	}
}
