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
	// down: url -> time of the last connect/poll failure. A member counts
	// as down for DownTTL after its last failure; a successful poll (Set)
	// clears it immediately. MarkDownIfTracked needs consecutive failures:
	// one dropped probe packet under load must not evict a healthy member
	// (observed as a ~1-in-5 flake in the CI matrix under -race load).
	down       map[string]time.Time
	failStreak map[string]int
	// queueMs: moving average of engine-reported queue wait per backend
	// (from the per-response stats block). Feeds queue-aware affinity.
	queueMs map[string]float64
}

// DownTTL: how long one failure keeps a member out of rotation. The
// poller re-marks a still-dead member on every failed poll, so it stays
// out continuously; a member whose failure goes unrepeated becomes
// eligible again after the TTL (a natural re-probe).
const DownTTL = 15 * time.Second

func NewTracker(w Weights) *Tracker {
	return &Tracker{
		loads: map[string]*Load{}, baseline: map[string][2]int{},
		inFlight: map[string]int{}, weights: w, down: map[string]time.Time{},
		failStreak: map[string]int{}, queueMs: map[string]float64{},
	}
}

// SetWeights swaps the scoring weights in place, keeping every load
// snapshot, down mark, in-flight count and queue average (a config save
// must not make busy GPUs look idle until the next poll).
func (t *Tracker) SetWeights(w Weights) {
	t.mu.Lock()
	t.weights = w
	t.mu.Unlock()
}

// queueAlpha: weight of the newest sample in the queue-wait average
// (≈ the last 5–10 requests dominate).
const queueAlpha = 0.3

// ObserveQueueWait folds one request's engine-reported queue wait (ms) into
// the backend's moving average.
func (t *Tracker) ObserveQueueWait(url string, ms float64) {
	if ms < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.queueMs[url]; ok {
		t.queueMs[url] = cur + queueAlpha*(ms-cur)
	} else {
		t.queueMs[url] = ms
	}
}

// QueueWait: the backend's recent average queue wait in ms (0 if unknown).
func (t *Tracker) QueueWait(url string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.queueMs[url]
}

func (t *Tracker) Set(url string, l *Load) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loads[url] = l
	delete(t.down, url) // a successful poll proves the member is back
	delete(t.failStreak, url)
}

// MarkDown records a connect or poll failure for url.
func (t *Tracker) MarkDown(url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.down[url] = time.Now()
	delete(t.failStreak, url)
}

// MarkDownIfTracked marks url down only if it has been polled
// successfully before. Poll failures on backends that never answered the
// metrics endpoints (non-NInfer/vLLM engines) must not evict them.
// MarkDownIfTracked: mark a previously-seen member down, but only after
// two consecutive poll failures — a single dropped probe (GC pause, port
// contention, scheduler hiccup) must not evict a healthy member.
func (t *Tracker) MarkDownIfTracked(url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.loads[url]; !ok {
		return // never polled successfully: nothing to mark
	}
	t.failStreak[url]++
	if t.failStreak[url] >= 2 {
		t.down[url] = time.Now()
	}
}

// SnapshotLoads: copy of current loads (diagnostics).
func (t *Tracker) SnapshotLoads() map[string]*Load {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]*Load, len(t.loads))
	for k, v := range t.loads {
		out[k] = v
	}
	return out
}

// IsDown reports whether url failed within the last DownTTL.
func (t *Tracker) IsDown(url string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.down[url]
	return ok && time.Since(at) < DownTTL
}

func (t *Tracker) Get(url string) *Load {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loads[url]
}

// Snapshot copies the current load map (metrics views iterate it without
// holding the tracker lock).
func (t *Tracker) Snapshot() map[string]*Load {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]*Load, len(t.loads))
	for u, l := range t.loads {
		out[u] = l
	}
	return out
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
	// Lane-based engines (NInfer, llama.cpp) report real lanes and a queue.
	if l.Engine == "ninfer" || l.Engine == "llamacpp" {
		lanes := l.Lanes
		if lanes < 1 {
			lanes = 1
		}
		laneP := min(float64(l.Running)/float64(lanes), 1.0)
		// A queue means the engine can't start more work right now, whatever
		// its nominal lanes say (device state slots or KV can be the real
		// limit: e.g. 5 lanes but 2 device slots queues at 2 running).
		if l.Waiting > 0 {
			laneP = 1.0
		}
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
	MaxContext     int // tokens this member can hold (prompt + output); 0 = unknown
	CapacityWeight int // 0 => use lanes from tracker (or 1)
	Lanes          int // resolved lane count for capacity math
}

// FitContext keeps the members whose context window can hold `need`
// tokens (prompt estimate + requested output). Members with an unknown
// window are kept. If nobody fits, all members are returned: the engine
// then rejects with its own, more precise error rather than the gateway
// guessing from a chars/4 estimate. Every member that can physically
// serve a request is a candidate, and cache affinity applies at every
// prompt size.
func FitContext(members []Member, need int) []Member {
	if need <= 0 {
		return members
	}
	fit := make([]Member, 0, len(members))
	for _, m := range members {
		if m.MaxContext == 0 || m.MaxContext >= need {
			fit = append(fit, m)
		}
	}
	if len(fit) == 0 {
		return members
	}
	return fit
}

// DayShare: rolling per-member count of NEW conversations routed today
// (first turns only). Feeds the fairness cap.
type DayShare struct {
	mu     sync.Mutex
	day    string
	counts map[string]int
}

func NewDayShare() *DayShare {
	return &DayShare{counts: map[string]int{}, day: time.Now().UTC().Format("2006-01-02")}
}

func (d *DayShare) rolloverLocked() {
	today := time.Now().UTC().Format("2006-01-02")
	if d.day != today {
		d.day = today
		d.counts = map[string]int{}
	}
}

// Inc: count a new conversation on member.
func (d *DayShare) Inc(member string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rolloverLocked()
	d.counts[member]++
}

// Counts: today's per-member conversation counts.
func (d *DayShare) Counts() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rolloverLocked()
	out := make(map[string]int, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// PickNewConversation: pick where a NEW conversation starts, honoring the
// fairness cap. A member over maxShare (share of new-conversations today)
// is deprioritized unless every member is over (then least-bad wins).
// Returns the chosen member URL or "" when the cap is not active.
func PickNewConversation(share *DayShare, maxShare float64, members []Member, scores map[string]float64) string {
	if share == nil || maxShare <= 0 || len(members) < 2 {
		return ""
	}
	counts := share.Counts()
	total := 0
	for _, v := range counts {
		total += v
	}
	if total == 0 {
		return ""
	}
	over := func(url string) bool {
		return float64(counts[url])/float64(total) > maxShare
	}
	var under []Member
	for _, m := range members {
		if !over(m.URL) {
			under = append(under, m)
		}
	}
	if len(under) == 0 {
		return "" // everyone over: fall back to normal scoring
	}
	// Among under-cap members, lowest score wins (ties: fewer convos).
	best := ""
	bestScore := 2.0
	bestCount := 1 << 30
	for _, m := range under {
		sc := scores[m.URL]
		if sc < bestScore || (sc == bestScore && counts[m.URL] < bestCount) {
			best, bestScore, bestCount = m.URL, sc, counts[m.URL]
		}
	}
	return best
}

// PickResult is the selected member plus the score map used.
type PickResult struct {
	URL     string
	ModelID string
	Scores  map[string]float64
	// CacheHit: true when content affinity chose the member (diagnostics).
	CacheHit   bool
	CacheDepth int
}

// PickCache: content-affinity pick. Consults the conversation table and
// honors the match only if the target GPU has headroom — guards scale with
// match strength (a deep prefix is worth keeping; a shallow one isn't
// worth piling onto a busy card). Returns nil when there's no usable match.
//
// guards: depth>=3 match released above 0.90; depth==2 above 0.50 (a
// thousand scraper sessions sharing an opener must not pile onto one GPU).
// Large prompts are filtered by the caller's context-fit pass (FitContext)
// and never reach here.
// coldPrefillMs is the caller's estimate of what moving this conversation to
// another GPU would cost (re-prefilling it there). When the pinned GPU is
// queueing for longer than that and another member has a free lane, the
// cache hit is declined and the conversation moves. 0 disables the check.
func PickCache(ct *CacheTable, poolName string, members []Member, tr *Tracker,
	msgs []Message, inFlightByURL map[string]int, coldPrefillMs float64) *PickResult {
	if ct == nil || len(msgs) < 2 || len(members) == 0 {
		return nil
	}
	url, depth, ok := ct.Lookup(poolName, msgs)
	if !ok {
		return nil
	}
	var picked *Member
	for i := range members {
		if members[i].URL == url {
			picked = &members[i]
			break
		}
	}
	if picked == nil {
		return nil // member vanished from the pool
	}
	guard := 0.90
	if depth <= 2 {
		guard = 0.50
	}
	score := tr.Score(url)
	// Blend in-flight so concurrent bursts count even before /slots polls.
	if l := tr.Get(url); l != nil {
		lanes := l.Lanes
		if lanes < 1 {
			lanes = 6
		}
		blended := float64(l.Running+inFlightByURL[url]) / float64(lanes)
		if blended > score {
			score = blended
		}
	}
	if score >= guard {
		return nil // cache hit but GPU too busy: fall through to score-based pick
	}
	// Queue-aware: a lane-based score can sit just under the guard while the
	// engine queues for seconds. Moving costs one cold prefill elsewhere;
	// staying costs the queue. Move when staying is clearly worse.
	if coldPrefillMs > 0 {
		if l := tr.Get(url); l != nil && l.Waiting > 0 && tr.QueueWait(url) > coldPrefillMs &&
			hasFreeLane(members, url, tr, inFlightByURL) {
			return nil
		}
	}
	return &PickResult{URL: picked.URL, ModelID: picked.ModelID, CacheHit: true, CacheDepth: depth}
}

// hasFreeLane: some member other than `except` has an idle lane right now.
func hasFreeLane(members []Member, except string, tr *Tracker, inFlightByURL map[string]int) bool {
	for _, m := range members {
		if m.URL == except {
			continue
		}
		l := tr.Get(m.URL)
		if l == nil || l.Lanes < 1 {
			continue
		}
		if l.Waiting == 0 && l.Running+inFlightByURL[m.URL] < l.Lanes {
			return true
		}
	}
	return false
}

// PickPool implements SPEC §5.4 deterministically.
// leader is the current leader URL ("" if none); it is updated by the caller
// from the returned URL.
func PickPool(poolName string, poolThreshold, stickyBias float64,
	capacityBias float64, members []Member, tr *Tracker, session, leader string,
	affinityMode bool) PickResult {

	scores := map[string]float64{}
	for _, m := range members {
		scores[m.URL] = tr.Score(m.URL)
	}

	// 2. Session pinning — skipped in affinityMode: blind md5 pinning
	// fights content affinity (it pins new conversations to whichever
	// member their key hashes to, regardless of where their cache lives).
	if session != "" && !affinityMode {
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
			return PickResult{URL: pinned.URL, ModelID: pinned.ModelID, Scores: scores}
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
		return PickResult{URL: best.URL, ModelID: best.ModelID, Scores: scores}
	}

	// 7. Choice: min by (score - sticky, original index). Members that
	// cannot fit the request were already removed by FitContext.
	// Ties (e.g. every member idle at score 0) go to the larger member —
	// more lanes or a higher capacity_weight — not to config order, so new
	// conversations start on the GPU with the most headroom.
	bestIdx := -1
	bestScore := 2.0
	const eps = 1e-9
	for i, m := range eligible {
		s := scores[m.URL]
		if m.URL == leader {
			s -= stickyBias
		}
		if s < bestScore-eps || (s <= bestScore+eps && bestIdx >= 0 && memberCapacity(m) > memberCapacity(eligible[bestIdx])) {
			bestScore = s
			bestIdx = i
		}
	}
	chosen := eligible[bestIdx]
	return PickResult{URL: chosen.URL, ModelID: chosen.ModelID, Scores: scores}
}

// md5Mod returns int(md5hex(s), 16) mod n (pin hashing).
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

// memberCapacity: a member's relative size for tie-breaking —
// capacity_weight when configured, else its lane count.
func memberCapacity(m Member) int {
	if m.CapacityWeight > 0 {
		return m.CapacityWeight
	}
	return m.Lanes
}
