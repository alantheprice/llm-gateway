package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTmp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "conf.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const fullConf = `{
  "gateway": {"port": 8035, "trust_local_networks": true},
  "discovery": {"local_ports": [8000], "remote_host": "192.168.1.100", "remote_ports": [8006]},
  "local_networks": ["192.168.1.0/24", "10.0.0.0/8"],
  "metrics": {"poll_interval": 10, "stale_threshold": 30, "default_max_seqs": 3,
              "backend_max_seqs": {"http://127.0.0.1:8000": 6}},
  "model_pools": {"qwen": {"members": [
      {"model_id": "qwen-a", "backend": "http://127.0.0.1:8000"},
      {"model_id": "qwen-b", "backend": "http://192.168.1.100:8006", "large_context": true, "capacity_weight": 8}
    ], "overflow_threshold": 0.2, "sticky_bias": 0.05, "large_prompt_tokens": 48000, "capacity_bias": 0.15}},
  "overflow_pairs": {"qwen-a": {"fallback_model_id": "qwen-b", "fallback_backend": "http://192.168.1.100:8006", "overflow_threshold": 0.2}},
  "public_models": ["qwen"]
}`

func TestLoadFull(t *testing.T) {
	p := writeTmp(t, fullConf)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Port != 8035 {
		t.Errorf("port = %d, want 8035", c.Gateway.Port)
	}
	if !c.Gateway.TrustLocalNetworks {
		t.Error("trust_local_networks should be true")
	}
	if len(c.ModelPools["qwen"].Members) != 2 {
		t.Fatalf("pool members = %d, want 2", len(c.ModelPools["qwen"].Members))
	}
	m := c.ModelPools["qwen"].Members[1]
	if !m.LargeContext || m.CapacityWeight != 8 {
		t.Errorf("member[1] large_context/weight = %v/%d", m.LargeContext, m.CapacityWeight)
	}
	if c.MaxSeqsFor("http://127.0.0.1:8000") != 6 {
		t.Errorf("MaxSeqsFor local = %d, want 6", c.MaxSeqsFor("http://127.0.0.1:8000"))
	}
	if c.MaxSeqsFor("http://192.168.1.100:8006") != 3 {
		t.Errorf("MaxSeqsFor remote default = %d, want 3", c.MaxSeqsFor("http://192.168.1.100:8006"))
	}
}

func TestDefaults(t *testing.T) {
	p := writeTmp(t, `{}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Port != 8033 {
		t.Errorf("default port = %d, want 8033", c.Gateway.Port)
	}
	if c.Metrics.NinferLaneWeight != 0.75 || c.Metrics.NinferQueueWeight != 0.15 || c.Metrics.NinferPressureWt != 0.10 {
		t.Errorf("default ninfer weights wrong: %v %v %v", c.Metrics.NinferLaneWeight, c.Metrics.NinferQueueWeight, c.Metrics.NinferPressureWt)
	}
	if c.Cache.TTL != 60 {
		t.Errorf("default cache ttl = %d, want 60", c.Cache.TTL)
	}
}

func TestPollWatch(t *testing.T) {
	p := writeTmp(t, fullConf)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	c.OnReload(func(nc *Config) { fired++ })

	if _, changed := c.PollWatch(); changed {
		t.Fatal("no change should be reported before file modification")
	}
	// Rewrite with a later mtime.
	if err := os.WriteFile(p, []byte(`{"gateway":{"port":9001}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := os.FileInfo(nil)
	_ = future
	// Force mtime forward to defeat coarse fs timestamps.
	fi, _ := os.Stat(p)
	tt := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(p, tt, tt); err != nil {
		t.Fatal(err)
	}
	nc, changed := c.PollWatch()
	if !changed {
		t.Fatal("expected reload after mtime bump")
	}
	if nc.Gateway.Port != 9001 {
		t.Errorf("reloaded port = %d, want 9001", nc.Gateway.Port)
	}
	if fired != 1 {
		t.Errorf("OnReload fired %d times, want 1", fired)
	}
}
