package server

import (
	"fmt"
	"sort"
	"strconv"

	"llmgateway/internal/config"
)

// A service is one engine (host + port). A GPU is a physical card. Several
// services can share a card, and one service can span several cards.
// Engines report the energy of the card(s) they run on, so two services on
// one card each report that card's full draw. Services that share cards
// (directly or through a service spanning both) form a GPU group; a group's
// energy is counted once, from its largest report.

// gpuGroup: services sharing a set of cards on one host.
type gpuGroup struct {
	Host     string   // cost-config host label ("" if unclaimed)
	GPUs     []string // card names; empty when the host lists no cards
	Services []string // backend URLs, sorted
	Mapped   bool     // from the host's gpus list, not assumed
}

// hostIndexOf: which configured host claims backend (-1 if none).
func hostIndexOf(hosts []config.HostCfg, backend string) int {
	key := backendHostIP(backend)
	for i, h := range hosts {
		for _, k := range h.HostKeys() {
			if k == key {
				return i
			}
		}
	}
	return -1
}

// gpuNames: display names of a host's cards by index ("GPU 1" when unnamed).
func gpuNames(h config.HostCfg, idx []int) []string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		n := h.GPUs[i].Name
		if n == "" {
			n = "GPU " + strconv.Itoa(i+1)
		}
		out = append(out, n)
	}
	return out
}

// groupServices partitions backends into GPU groups. A service the config
// doesn't place on any card is its own group (assumed to have its own card).
func groupServices(hosts []config.HostCfg, backends []string) []gpuGroup {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		r := find(parent[x])
		parent[x] = r
		return r
	}
	union := func(a, b string) { parent[find(a)] = find(b) }

	svcNode := func(b string) string { return "svc:" + b }
	for _, b := range backends {
		find(svcNode(b))
		hi := hostIndexOf(hosts, b)
		if hi < 0 {
			continue
		}
		for _, gi := range hosts[hi].GPUsFor(backendPort(b)) {
			union(svcNode(b), fmt.Sprintf("gpu:%d/%d", hi, gi))
		}
	}

	byRoot := map[string]*gpuGroup{}
	gpuIdx := map[string]map[int]bool{}
	var order []string
	for _, b := range backends {
		r := find(svcNode(b))
		g := byRoot[r]
		if g == nil {
			g = &gpuGroup{}
			byRoot[r] = g
			gpuIdx[r] = map[int]bool{}
			order = append(order, r)
		}
		g.Services = append(g.Services, b)
		if hi := hostIndexOf(hosts, b); hi >= 0 {
			g.Host = hosts[hi].Label
			for _, gi := range hosts[hi].GPUsFor(backendPort(b)) {
				gpuIdx[r][gi] = true
				g.Mapped = true
			}
		}
	}
	out := make([]gpuGroup, 0, len(order))
	for _, r := range order {
		g := byRoot[r]
		sort.Strings(g.Services)
		if g.Mapped {
			hi := hostIndexOf(hosts, g.Services[0])
			idx := make([]int, 0, len(gpuIdx[r]))
			for i := range gpuIdx[r] {
				idx = append(idx, i)
			}
			sort.Ints(idx)
			g.GPUs = gpuNames(hosts[hi], idx)
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Services[0] < out[j].Services[0]
	})
	return out
}

// energyReporters: for each group, the one service whose energy report
// counts (the largest kWh today; ties by URL). Sum energy only over these.
func energyReporters(groups []gpuGroup, kwh map[string]float64) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		best, bestKwh := "", -1.0
		for _, b := range g.Services {
			k, ok := kwh[b]
			if !ok {
				continue
			}
			if k > bestKwh || (k == bestKwh && b < best) {
				best, bestKwh = b, k
			}
		}
		if best != "" {
			out[best] = true
		}
	}
	return out
}

// energyWarnings: hosts whose cards aren't listed but that run several
// energy-reporting services, which may share a card (and be counted twice).
func energyWarnings(hosts []config.HostCfg, groups []gpuGroup, kwh map[string]float64) []string {
	reporting := map[string]int{}
	for _, g := range groups {
		if g.Mapped || g.Host == "" {
			continue
		}
		for _, b := range g.Services {
			if kwh[b] > 0 {
				reporting[g.Host]++
			}
		}
	}
	var out []string
	for _, h := range hosts {
		if n := reporting[h.Label]; n > 1 && len(h.GPUs) == 0 {
			out = append(out, fmt.Sprintf("%s runs %d services that report GPU energy. If any share a card, list the host's GPUs and their services in Config → Costs & hosts, or that card's energy is counted more than once.", h.Label, n))
		}
	}
	return out
}

// splitGroupEnergy: each service's share of its group's energy, by the
// tokens it served (evenly when the group served none).
func splitGroupEnergy(g gpuGroup, groupKwh float64, tokens map[string]float64) map[string]float64 {
	out := map[string]float64{}
	var total float64
	for _, b := range g.Services {
		total += tokens[b]
	}
	for _, b := range g.Services {
		if total > 0 {
			out[b] = groupKwh * tokens[b] / total
		} else {
			out[b] = groupKwh / float64(len(g.Services))
		}
	}
	return out
}

// countedEnergy: s's current GPU groups, and which backends' energy counts.
// usage is the engines' /usage payloads by backend.
func (s *Server) countedEnergy(usage map[string]map[string]any) ([]gpuGroup, map[string]bool, map[string]float64) {
	s.mu.Lock()
	hosts := s.cfg.Hosts
	s.mu.Unlock()
	kwh := map[string]float64{}
	backends := make([]string, 0, len(usage))
	for b, u := range usage {
		backends = append(backends, b)
		if en, ok := u["energy"].(map[string]any); ok && en != nil {
			kwh[b] = usageNum(u, "energy", "today", "kwh")
		}
	}
	groups := groupServices(hosts, backends)
	return groups, energyReporters(groups, kwh), kwh
}

func (s *Server) hostsSnapshot() []config.HostCfg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Hosts
}

// gpuEnergyRows: the Costs page's GPU list — each group of cards with the
// services on it and the energy counted for it today.
func (s *Server) gpuEnergyRows(groups []gpuGroup, reporters map[string]bool, kwh map[string]float64,
	usage map[string]map[string]any, routed map[string]float64) []map[string]any {
	out := []map[string]any{}
	for _, g := range groups {
		var gk, gc float64
		reporter := ""
		for _, b := range g.Services {
			if reporters[b] {
				reporter = b
				gk, gc = kwh[b], usageNum(usage[b], "energy", "today", "cost_usd")
			}
		}
		share := splitGroupEnergy(g, gk, routed)
		svcs := make([]map[string]any, 0, len(g.Services))
		for _, b := range g.Services {
			id := s.gpuIdentity(b)
			svcs = append(svcs, map[string]any{"backend": b, "label": id.Label, "models": s.modelsOf(b),
				"kwh_share": share[b], "routed_tokens": routed[b], "reports_energy": b == reporter})
		}
		out = append(out, map[string]any{
			"host": g.Host, "gpus": g.GPUs, "mapped": g.Mapped, "services": svcs,
			"kwh_today": gk, "cost_usd_today": gc, "reporter": reporter,
		})
	}
	return out
}

// attributeServiceEnergy: per-service rows carry their share of the group
// energy (not the whole card's), the card(s) they run on, and rows for
// services on a shared card that report no energy of their own.
func attributeServiceEnergy(hosts []config.HostCfg, rows []map[string]any, groups []gpuGroup,
	kwh map[string]float64, routed map[string]float64) []map[string]any {
	rowOf := map[string]map[string]any{}
	for _, r := range rows {
		if bs, ok := r["backends"].([]string); ok {
			for _, b := range bs {
				rowOf[b] = r
			}
		}
	}
	for _, g := range groups {
		if g.Mapped {
			for _, b := range g.Services {
				if r := rowOf[b]; r != nil {
					r["gpus"] = g.GPUs
				}
			}
		}
		if len(g.Services) < 2 {
			continue
		}
		var gk float64
		for _, b := range g.Services {
			gk = max(gk, kwh[b])
		}
		share := splitGroupEnergy(g, gk, routed)
		for _, b := range g.Services {
			r := rowOf[b]
			if r == nil {
				id := identifyGPU(hosts, b)
				r = map[string]any{"gpu_key": id.Key, "gpu_label": id.Label, "host": id.Host, "via": id.Via,
					"backend": b, "backends": []string{b}, "tokens": int64(routed[b]), "cache_hits": int64(0), "engine_input": int64(0)}
				if g.Mapped {
					r["gpus"] = g.GPUs
				}
				rows = append(rows, r)
				rowOf[b] = r
			}
			r["kwh"] = share[b]
			r["kwh_shared"] = true
		}
	}
	return rows
}

// modelsOf: the model ids a service (LAN or link engine) serves.
func (s *Server) modelsOf(u string) []string {
	if isLinkURL(u) {
		if info := s.linkBackendInfo(u); info != nil {
			return info.Models
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if info := s.backends[u]; info != nil {
		return append([]string(nil), info.Models...)
	}
	return nil
}
