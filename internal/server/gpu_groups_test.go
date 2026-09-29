package server

import (
	"math"
	"reflect"
	"testing"

	"llmgateway/internal/config"
)

// Two cards, three services: 8006 on card 1; 8001 and 8009 share card 2.
// A single-card host that lists no services puts them all on that card.
func TestGroupServicesByCard(t *testing.T) {
	hosts := []config.HostCfg{
		{Label: "ws", Links: []string{"ws6000"}, GPUs: []config.GPUCfg{
			{Name: "RTX PRO 6000", Services: []string{"8006"}},
			{Name: "RTX 4090", Services: []string{"8001", ":8009"}},
		}},
		{Label: "box", IPs: []string{"127.0.0.1"}, GPUs: []config.GPUCfg{{Name: "RTX 5090"}}},
		{Label: "old", IPs: []string{"10.0.0.9"}},
	}
	backends := []string{"http://link/ws6000:8006", "http://link/ws6000:8001", "http://link/ws6000:8009",
		"http://127.0.0.1:8000", "http://127.0.0.1:8002", "http://10.0.0.9:8000", "http://10.0.0.9:8001"}
	got := groupServices(hosts, backends)
	var summary [][]string
	for _, g := range got {
		summary = append(summary, append(append([]string{g.Host}, g.GPUs...), g.Services...))
	}
	want := [][]string{
		{"box", "RTX 5090", "http://127.0.0.1:8000", "http://127.0.0.1:8002"},
		{"old", "http://10.0.0.9:8000"},
		{"old", "http://10.0.0.9:8001"},
		{"ws", "RTX 4090", "http://link/ws6000:8001", "http://link/ws6000:8009"},
		{"ws", "RTX PRO 6000", "http://link/ws6000:8006"},
	}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("groups:\n got %v\nwant %v", summary, want)
	}

	// Energy counts once per group; unlisted hosts with two reporters warn.
	kwh := map[string]float64{"http://127.0.0.1:8000": 4, "http://127.0.0.1:8002": 3.9,
		"http://10.0.0.9:8000": 1, "http://10.0.0.9:8001": 1, "http://link/ws6000:8006": 5}
	rep := energyReporters(got, kwh)
	if !rep["http://127.0.0.1:8000"] || rep["http://127.0.0.1:8002"] || !rep["http://link/ws6000:8006"] ||
		!rep["http://10.0.0.9:8000"] || !rep["http://10.0.0.9:8001"] || len(rep) != 4 {
		t.Fatalf("reporters = %v", rep)
	}
	if w := energyWarnings(hosts, got, kwh); len(w) != 1 || w[0][:3] != "old" {
		t.Fatalf("warnings = %v", w)
	}

	// A shared card's energy splits by tokens served, evenly when idle.
	sh := splitGroupEnergy(got[3], 2, map[string]float64{"http://link/ws6000:8001": 1, "http://link/ws6000:8009": 3})
	if math.Abs(sh["http://link/ws6000:8001"]-0.5) > 1e-9 || math.Abs(sh["http://link/ws6000:8009"]-1.5) > 1e-9 {
		t.Fatalf("split = %v", sh)
	}
	if sh := splitGroupEnergy(got[3], 2, nil); sh["http://link/ws6000:8001"] != 1 {
		t.Fatalf("idle split = %v", sh)
	}
}

// A service spanning two cards joins them into one group.
func TestGroupServicesSpanningCards(t *testing.T) {
	hosts := []config.HostCfg{{Label: "tp", IPs: []string{"10.0.0.5"}, GPUs: []config.GPUCfg{
		{Name: "A", Services: []string{"8000", "8001"}}, {Name: "B", Services: []string{"8000"}}, {Name: "C", Services: []string{"8002"}},
	}}}
	got := groupServices(hosts, []string{"http://10.0.0.5:8000", "http://10.0.0.5:8001", "http://10.0.0.5:8002"})
	if len(got) != 2 || !reflect.DeepEqual(got[0].GPUs, []string{"A", "B"}) || len(got[0].Services) != 2 ||
		!reflect.DeepEqual(got[1].GPUs, []string{"C"}) {
		t.Fatalf("groups = %+v", got)
	}
}
