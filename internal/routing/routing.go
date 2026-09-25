// Package routing implements engine-aware load scoring, session pinning,
// and pool member selection. See docs/SPEC.md §5–6.
package routing

import (
	"crypto/md5"
	"math/big"
	"sync"
	"time"
)

// Load is the per-backend metric snapshot (SPEC §5.3, §6).
type Load struct {
	Engine string // "ninfer" | "vllm"

	Running int
	Waiting int
	Lanes   int
	MaxSeqs int
	KVUsage float64 // vLLM only

	SpillsTotal    int
	EvictionsTotal int

	DecodeTPS   float64
	PrefillTPS  float64
	EnergyKWH   float64
	CacheHitPct float64
	LastUpdated time.Time
}

// Weights holds the tunables from config.metrics.
type Weights struct {
	NinferLane     float64
	NinferQueue    float64
	NinferPressure float64
	StaleSeconds   float64
}

func DefaultWeights() Weights {
	return Weights{NinferLane: 0.75, NinferQueue: 0.15, NinferPressure: 0.10, StaleSeconds: 30}
}

// Tracker keeps load snapshots + spill/eviction baselines + in-flight counts.
type Tracker struct {
	mu       sync.Mutex
	loads    map[string]*Load
	baseline map[string][2]int // url -> {spills, evictions} at last score
	inFlight map[string]int
	weights  Weights
}

func NewTracker(w Weights) *Tracker {
	return &Tracker{
		loads: map[string]*Load{}, baseline: map[string][2]int{},
		inFlight: map[string]int{}, weights: w,
	}
}

func (t *Tracker) Set(url string, l *Load) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loads[url] = l
}

func (t *Tracker) Get(url string) *Load {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loads[url]
}

func (t *Tracker) InFlightInc(url string) { t.mu.Lock(); t.inFlight[url]++; t.mu.Unlock() }
func (t *Tracker) InFlightDec(url string) {
	t.mu.Lock()
	if t.inFlight[url] > 0 {
		t.inFlight[url]--
	}
	t.mu.Unlock()
}
func (t *Tracker) InFlight(url string) int { t.mu.Lock(); defer t.mu.Unlock(); return t.inFlight[url] }

// Score returns 0..1 per SPEC §5.3. Stale/missing => 0.0.
func (t *Tracker) Score(url string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.loads[url]
	if l == nil {
		return 0.0
	}
	if !l.LastUpdated.IsZero() && time.Since(l.LastUpdated).Seconds() > t.weights.StaleSeconds {
		return 0.0
	}
	if l.Engine == "ninfer" {
		lanes := l.Lanes
		if lanes < 1 {
			lanes = 1
		}
		laneP := min(float64(l.Running)/float64(lanes), 1.0)
		queueP := min(float64(l.Waiting)/max(float64(lanes)/2, 1), 1.0)

		kvP := 0.0
		cur := [2]int{l.SpillsTotal, l.EvictionsTotal}
		if prev, ok := t.baseline[url]; ok {
			if cur[0] > prev[0] || cur[1] > prev[1] {
				kvP = 1.0
			}
		}
		t.baseline[url] = cur

		s := laneP*t.weights.NinferLane + queueP*t.weights.NinferQueue + kvP*t.weights.NinferPressure
		return clamp01(s)
	}
	// vLLM heuristic
	maxSeqs := l.MaxSeqs
	if maxSeqs < 1 {
		maxSeqs = 3
	}
	s := (float64(l.Running)/float64(maxSeqs))*0.6 + l.KVUsage*0.3 + (min(float64(l.Waiting), 5)/5)*0.1
	return clamp01(s)
}

// Member is one pool member for selection.
type Member struct {
	URL            string
	ModelID        string
	LargeContext   bool
	CapacityWeight int // 0 => use lanes from tracker (or 1)
	Lanes          int // resolved lane count for capacity math
}

// PickResult is the selected member plus the score map used.
type PickResult struct {
	URL     string
	ModelID string
	Scores  map[string]float64
}

// PickPool implements SPEC §5.4 deterministically.
// leader is the current leader URL ("" if none); it is updated by the caller
// from the returned URL.
func PickPool(poolName string, poolThreshold, stickyBias float64, largePromptTokens int,
	capacityBias float64, members []Member, tr *Tracker, estTokens int, session, leader string) PickResult {

	scores := map[string]float64{}
	for _, m := range members {
		scores[m.URL] = tr.Score(m.URL)
	}

	// 2. Session pinning
	if session != "" {
		idx := md5Mod(len(members), poolName+"|"+session)
		pinned := members[idx]
		l := tr.Get(pinned.URL)
		pinnedRunning, pinnedLanes := 0, 6
		if l != nil {
			pinnedRunning = l.Running
			if l.Lanes > 0 {
				pinnedLanes = l.Lanes
			}
		}
		pinnedRunning += tr.InFlight(pinned.URL)
		if pinnedRunning+1 <= pinnedLanes && scores[pinned.URL] < 0.90 {
			return PickResult{pinned.URL, pinned.ModelID, scores}
		}
	}

	// 3. In-flight blend
	for _, m := range members {
		infl := tr.InFlight(m.URL)
		if infl == 0 {
			continue
		}
		l := tr.Get(m.URL)
		if l == nil {
			continue
		}
		lanes := l.Lanes
		if lanes < 1 {
			lanes = 6
		}
		blended := min(float64(l.Running+infl)/float64(lanes), 1.0)
		scores[m.URL] = max(scores[m.URL], blended)
	}

	// 4. Capacity scaling (only when lane counts differ)
	if capacityBias > 0 {
		laneCounts := map[string]int{}
		for _, m := range members {
			if m.CapacityWeight > 0 {
				laneCounts[m.URL] = m.CapacityWeight
				continue
			}
			if m.Lanes > 0 {
				laneCounts[m.URL] = m.Lanes
				continue
			}
			laneCounts[m.URL] = 1
		}
		maxLanes := 0
		differs := false
		first := true
		var fv int
		for _, v := range laneCounts {
			if v > maxLanes {
				maxLanes = v
			}
			if first {
				fv = v
				first = false
			} else if v != fv {
				differs = true
			}
		}
		if maxLanes > 0 && differs {
			for url := range scores {
				scale := 1.0 + capacityBias*(1.0-float64(laneCounts[url])/float64(maxLanes))
				scores[url] = min(scores[url]*scale, 1.0)
			}
		}
	}

	wantsLarge := largePromptTokens > 0 && estTokens >= largePromptTokens

	// 5a. Large-prompt fast path
	if wantsLarge {
		bestLarge := pickLarge(members, scores, 0.90)
		if bestLarge != nil {
			return PickResult{bestLarge.URL, bestLarge.ModelID, scores}
		}
	}

	// 6. Threshold filter
	var eligible []Member
	for _, m := range members {
		if scores[m.URL] < poolThreshold {
			eligible = append(eligible, m)
		}
	}
	if len(eligible) == 0 {
		// least-bad single member
		best := members[0]
		for _, m := range members[1:] {
			if scores[m.URL] < scores[best.URL] {
				best = m
			}
		}
		return PickResult{best.URL, best.ModelID, scores}
	}

	// 7. Choice: min by (score + sizePenalty - sticky, original index)
	bestIdx := -1
	bestScore := 2.0
	for i, m := range eligible {
		s := scores[m.URL]
		if wantsLarge && !m.LargeContext {
			s += 0.50
		} else if !wantsLarge && m.LargeContext {
			s += 0.50
		}
		if m.URL == leader {
			s -= stickyBias
		}
		if s < bestScore {
			bestScore = s
			bestIdx = i
		}
	}
	chosen := eligible[bestIdx]
	return PickResult{chosen.URL, chosen.ModelID, scores}
}

func pickLarge(members []Member, scores map[string]float64, cap float64) *Member {
	var best *Member
	for i := range members {
		m := &members[i]
		if !m.LargeContext {
			continue
		}
		if scores[m.URL] >= cap {
			continue
		}
		if best == nil || scores[m.URL] < scores[best.URL] {
			best = m
		}
	}
	return best
}

// md5Mod returns int(md5hex(s), 16) mod n — Python parity for pin hashing.
func md5Mod(n int, s string) int {
	sum := md5.Sum([]byte(s))
	x := new(big.Int).SetBytes(sum[:])
	nn := big.NewInt(int64(n))
	x.Mod(x, nn)
	return int(x.Int64())
}

// EstimateTokens estimates prompt size per SPEC §5.1: chars/4 for text
// content (strings or content-part arrays), image_url parts count 2000.
func EstimateTokens(messages []map[string]any) int {
	total := 0
	for _, msg := range messages {
		switch c := msg["content"].(type) {
		case string:
			total += len([]rune(c)) / 4
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if pm["type"] == "image_url" {
					total += 2000
					continue
				}
				if txt, ok := pm["text"].(string); ok {
					total += len([]rune(txt)) / 4
				}
			}
		}
	}
	if total < 1 {
		total = 1
	}
	return total
}

func clamp01(f float64) float64 { return min(max(f, 0.0), 1.0) }
