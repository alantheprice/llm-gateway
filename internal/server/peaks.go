// Peak throughput memory for capacity-based pricing. The gateway watches
// every response's engine timings block (predicted_per_second /
// prompt_per_second, the same data the chat UI shows) and keeps the
// high-water mark per backend, bucketed by context size — decode speed
// varies with context, so "peak" is a curve, not a number. High-water
// marks decay (halve weekly of idle) so hardware changes or throttling
// don't anchor the pricing basis forever. Persisted to peaks.json.
package server

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ContextBuckets: decode tok/s depends on how full the context is; peaks
// are tracked per bucket so the pricing basis reflects the contexts users
// actually run.
var contextBuckets = []struct {
	label string
	max   float64 // prompt tokens upper bound (last bucket = +inf)
}{
	{"<8k", 8_000},
	{"8k-32k", 32_000},
	{"32k-128k", 128_000},
	{"128k+", math.Inf(1)},
}

func bucketFor(promptTokens float64) string {
	for _, b := range contextBuckets {
		if promptTokens < b.max {
			return b.label
		}
	}
	return "128k+"
}

// BackendPeak: high-water throughput for one engine backend.
type BackendPeak struct {
	TGPoS   float64            `json:"tg_tok_per_s_peak"`
	PPPoS   float64            `json:"pp_tok_per_s_peak"`
	Buckets map[string]float64 `json:"tg_by_context"` // bucket label -> peak tg
	Updated time.Time          `json:"updated"`
}

// PeakStore: per-backend peaks + persistence.
type PeakStore struct {
	mu       sync.Mutex
	path     string
	Backends map[string]*BackendPeak `json:"backends"`
	dirty    bool
	lastSave time.Time
}

func NewPeakStore(path string) *PeakStore {
	p := &PeakStore{path: path, Backends: map[string]*BackendPeak{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, p)
		if p.Backends == nil {
			p.Backends = map[string]*BackendPeak{}
		}
	}
	return p
}

// decay: a peak that hasn't been re-observed for 7+ days halves weekly.
func decayed(peak float64, updated time.Time, now time.Time) float64 {
	if updated.IsZero() {
		return 0
	}
	days := now.Sub(updated).Hours() / 24
	if days <= 7 {
		return peak
	}
	return peak * math.Pow(0.5, (days-7)/7)
}

// Observe records a response's measured throughput. promptTokens buckets
// the sample by context size.
func (p *PeakStore) Observe(backend string, promptTokens, tgTPS, ppTPS float64) {
	if backend == "" || (tgTPS <= 0 && ppTPS <= 0) {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	bp := p.Backends[backend]
	if bp == nil {
		bp = &BackendPeak{Buckets: map[string]float64{}, Updated: now}
		p.Backends[backend] = bp
	}
	// Bucket peaks never decay (they're the context curve); the backend
	// headline does (below) so the fleet basis stays current.
	if tgTPS > 0 {
		bkt := bucketFor(promptTokens)
		if tgTPS > bp.Buckets[bkt] {
			bp.Buckets[bkt] = tgTPS
		}
		if tgTPS > bp.TGPoS {
			bp.TGPoS = tgTPS
			bp.Updated = now
			p.dirty = true
		}
	}
	if ppTPS > bp.PPPoS {
		bp.PPPoS = ppTPS
		bp.Updated = now
		p.dirty = true
	}
	p.flushIfDueLocked(now)
}

// FleetPeaks returns the decayed fleet high-water decode + prefill rates
// (sum over backends — engines saturate independently).
func (p *PeakStore) FleetPeaks() (tg, pp float64) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, bp := range p.Backends {
		tg += decayed(bp.TGPoS, bp.Updated, now)
		pp += decayed(bp.PPPoS, bp.Updated, now)
	}
	return tg, pp
}

// Snapshot copies the peak map for the payload.
func (p *PeakStore) Snapshot() map[string]any {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{}
	for url, bp := range p.Backends {
		buckets := map[string]float64{}
		maxBucket := 0.0
		for k, v := range bp.Buckets {
			buckets[k] = math.Round(v*10) / 10
			if v > maxBucket {
				maxBucket = v
			}
		}
		out[url] = map[string]any{
			"tg_tok_per_s_peak":   math.Round(decayed(bp.TGPoS, bp.Updated, now)*10) / 10,
			"pp_tok_per_s_peak":   math.Round(bp.PPPoS*10) / 10,
			"tg_by_context":       buckets,
			"best_context_bucket": bucketLabelFor(maxBucket, bp.Buckets),
			"updated":             bp.Updated.Format(time.RFC3339),
		}
	}
	return out
}

func bucketLabelFor(max float64, buckets map[string]float64) string {
	for label, v := range buckets {
		if v == max {
			return label
		}
	}
	return ""
}

const peakFlushInterval = 60 * time.Second

func (p *PeakStore) flushIfDueLocked(now time.Time) {
	if !p.dirty || now.Sub(p.lastSave) < peakFlushInterval {
		return
	}
	p.saveLocked()
}

func (p *PeakStore) saveLocked() {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		if os.Rename(tmp, p.path) == nil {
			p.dirty = false
			p.lastSave = time.Now()
		}
	}
}

// Flush persists peaks now.
func (p *PeakStore) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

func peaksPath(usagePath string) string {
	return filepath.Join(filepath.Dir(usagePath), "peaks.json")
}
