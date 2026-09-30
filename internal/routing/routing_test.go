package routing

import (
	"math"
	"testing"
	"time"
)

func mkLoad(engine string, running, waiting, lanes int, spills, evictions int, age time.Duration) *Load {
	return &Load{
		Engine: engine, Running: running, Waiting: waiting, Lanes: lanes, MaxSeqs: lanes,
		SpillsTotal: spills, EvictionsTotal: evictions,
		LastUpdated: time.Now().Add(-age),
	}
}

func TestScoreIdleAllEngines(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("local", mkLoad("ninfer", 0, 0, 6, 0, 0, 0))
	tr.Set("remote", mkLoad("vllm", 0, 0, 8, 0, 0, 0))
	if s := tr.Score("local"); s != 0 {
		t.Errorf("ninfer idle score = %v, want 0", s)
	}
	if s := tr.Score("remote"); s != 0 {
		t.Errorf("vllm idle score = %v, want 0", s)
	}
}

func TestScoreNinferFormula(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	// 3/6 lanes running, 2 waiting, no new spills
	tr.Set("a", mkLoad("ninfer", 3, 2, 6, 100, 5, 0))
	// baseline seeded on first Score call => no pressure yet
	got := tr.Score("a")
	// Requests waiting => lanes count as full (laneP = 1);
	// queueP = min(waiting/(lanes/2), 1) = 2/3.
	want := 0.75*1.0 + 0.15*(2.0/3.0)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ninfer score = %v, want %v", got, want)
	}
	// Rising spills => +0.10 pressure
	tr.Set("a", mkLoad("ninfer", 3, 2, 6, 101, 5, 0))
	got = tr.Score("a")
	want += 0.10
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ninfer+pressure score = %v, want %v", got, want)
	}
}

func TestScoreVLLMFormula(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	l := mkLoad("vllm", 4, 2, 8, 0, 0, 0)
	l.KVUsage = 0.5
	tr.Set("v", l)
	want := (4.0/8.0)*0.6 + 0.5*0.3 + (2.0/5.0)*0.1
	if got := tr.Score("v"); math.Abs(got-want) > 1e-9 {
		t.Errorf("vllm score = %v, want %v", got, want)
	}
}

func TestScoreStaleIsIdle(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("old", mkLoad("ninfer", 6, 6, 6, 0, 0, 31*time.Second))
	if s := tr.Score("old"); s != 0 {
		t.Errorf("stale score = %v, want 0", s)
	}
	if s := tr.Score("unknown"); s != 0 {
		t.Errorf("unknown score = %v, want 0", s)
	}
}

func members() []Member {
	return []Member{
		{URL: "http://127.0.0.1:8000", ModelID: "qwen-a", Lanes: 6},
		{URL: "http://192.168.1.100:8006", ModelID: "qwen-b", CapacityWeight: 8, Lanes: 8},
	}
}

// Golden pinning map: md5("qwen|<session>") % 2
func TestSessionPinning(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 0, 0, 6, 0, 0, 0))
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	cases := map[string]string{ // session -> expected URL
		"alpha":            "http://192.168.1.100:8006", // idx 1
		"beta":             "http://127.0.0.1:8000",     // idx 0
		"gamma":            "http://127.0.0.1:8000",     // idx 0
		"session-9f8e7d6c": "http://192.168.1.100:8006", // idx 1
	}
	for sess, wantURL := range cases {
		got := PickPool("qwen", 0.20, 0.05, 0.15, members(), tr, sess, "", false)
		if got.URL != wantURL {
			t.Errorf("pin(%s) = %s, want %s", sess, got.URL, wantURL)
		}
	}
}

// Prompt size has no effect on selection: with a busy member over the pool
// threshold the eligible sibling wins, and with both idle the larger
// member (capacity weight) wins the tie.
func TestNoSizePenalty(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 6, 0, 6, 0, 0, 0))
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 2, 0, 8, 0, 0, 0))
	// local at 6/6 lanes (0.75+) is over the 0.20 threshold; remote
	// (2/8 lanes = 0.1875) is eligible and wins.
	got := PickPool("qwen", 0.20, 0.05, 0.15, members(), tr, "", "", false)
	if got.URL != "http://192.168.1.100:8006" {
		t.Errorf("-> %s, want the less-loaded remote", got.URL)
	}
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 0, 0, 6, 0, 0, 0))
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	got = PickPool("qwen", 0.20, 0.05, 0.15, members(), tr, "", "", false)
	if got.URL != "http://192.168.1.100:8006" {
		t.Errorf("both idle -> %s, want the larger member (capacity tie-break)", got.URL)
	}
}

// FitContext drops members whose window cannot hold the request, keeps
// unknown windows, and falls back to everyone when nobody fits.
func TestFitContext(t *testing.T) {
	ms := []Member{
		{URL: "small", MaxContext: 32000},
		{URL: "big", MaxContext: 262144},
		{URL: "unknown"},
	}
	urls := func(ms []Member) (out []string) {
		for _, m := range ms {
			out = append(out, m.URL)
		}
		return
	}
	if got := urls(FitContext(ms, 20000)); len(got) != 3 {
		t.Errorf("20K fits all: %v", got)
	}
	if got := urls(FitContext(ms, 100000)); len(got) != 2 || got[0] != "big" || got[1] != "unknown" {
		t.Errorf("100K -> %v, want [big unknown]", got)
	}
	known := ms[:2]
	if got := urls(FitContext(known, 500000)); len(got) != 2 {
		t.Errorf("nobody fits -> %v, want all (engine rejects precisely)", got)
	}
	if got := urls(FitContext(ms, 0)); len(got) != 3 {
		t.Errorf("need 0 -> %v, want all", got)
	}
}

func TestAllSaturatedFallsBackToLeastBad(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 6, 0, 6, 0, 0, 0))     // full, no queue: 0.75
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 7, 2, 8, 0, 0, 0)) // queueing: 0.75+0.15*0.5=0.825
	got := PickPool("qwen", 0.20, 0.05, 0, members(), tr, "", "", false)
	if got.URL != "http://127.0.0.1:8000" {
		t.Errorf("least-bad = %s, want the GPU that isn't queueing (0.75 < 0.825)", got.URL)
	}
}

func TestInFlightBlending(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 0, 0, 6, 0, 0, 0))
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	// 5 in flight locally: engine running lags, blend to 5/6 => max(score, 0.833)
	for i := 0; i < 5; i++ {
		tr.InFlightInc("http://127.0.0.1:8000")
	}
	got := PickPool("qwen", 0.20, 0.05, 0, members(), tr, "", "", false)
	if got.URL != "http://192.168.1.100:8006" {
		t.Errorf("burst should push to remote, got %s (scores %v)", got.URL, got.Scores)
	}
	if got.Scores["http://127.0.0.1:8000"] < 0.8 {
		t.Errorf("blended local score = %v, want >= ~0.833", got.Scores["http://127.0.0.1:8000"])
	}
}

func TestStickyBiasPrefersLeader(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	// Equal scores; leader discount should keep the leader.
	tr.Set("http://127.0.0.1:8000", mkLoad("ninfer", 0, 0, 6, 0, 0, 0))
	tr.Set("http://192.168.1.100:8006", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	// no capacity bias so scores stay equal
	got := PickPool("qwen", 0.20, 0.05, 0, members(), tr, "", "http://127.0.0.1:8000", false)
	if got.URL != "http://127.0.0.1:8000" {
		t.Errorf("leader should win ties, got %s", got.URL)
	}
}

func TestEstimateTokens(t *testing.T) {
	// chars/4 for text; images 2000; at least the sum of parts.
	msgs := []map[string]any{
		{"role": "user", "content": "abcdefgh"}, // 8 chars -> 2
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "abcd"},                               // 1
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}}, // 2000
		}},
	}
	if got := EstimateTokens(msgs); got != 2003 {
		t.Errorf("EstimateTokens = %d, want 2003", got)
	}
}

// Regression: a tracked member's poll failures must not panic. failStreak
// was added without being initialized in NewTracker, so the first failed
// poll of a healthy member crashed the gateway ("assignment to entry in
// nil map" in MarkDownIfTracked, 2026-09-27 22:01).
func TestMarkDownIfTrackedFreshTracker(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("u", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	tr.MarkDownIfTracked("u") // first failure: tolerated
	if tr.IsDown("u") {
		t.Fatal("one failed poll must not mark a member down")
	}
	tr.MarkDownIfTracked("u") // second consecutive failure
	if !tr.IsDown("u") {
		t.Fatal("two consecutive failures should mark the member down")
	}
	tr.Set("u", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	if tr.IsDown("u") {
		t.Fatal("a successful poll should clear down")
	}
}

// Equal scores go to the larger member (lanes / capacity_weight), not to
// config order.
func TestTieBreakPrefersCapacity(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	small := Member{URL: "small", ModelID: "m", Lanes: 5, CapacityWeight: 1}
	big := Member{URL: "big", ModelID: "m", Lanes: 8, CapacityWeight: 8}
	tr.Set("small", mkLoad("ninfer", 0, 0, 5, 0, 0, 0))
	tr.Set("big", mkLoad("ninfer", 0, 0, 8, 0, 0, 0))
	got := PickPool("qwen", 0.20, 0.05, 0.15, []Member{small, big}, tr, "", "", true)
	if got.URL != "big" {
		t.Fatalf("idle tie -> %s, want the larger member", got.URL)
	}
	// The sticky leader bonus still wins over capacity.
	got = PickPool("qwen", 0.20, 0.05, 0.15, []Member{small, big}, tr, "", "small", true)
	if got.URL != "small" {
		t.Fatalf("leader -> %s, want sticky leader", got.URL)
	}
}

func TestQueueWaitAverage(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	if tr.QueueWait("x") != 0 {
		t.Fatal("unknown backend should report 0")
	}
	tr.ObserveQueueWait("x", 1000)
	tr.ObserveQueueWait("x", 0)
	if q := tr.QueueWait("x"); q < 600 || q > 800 {
		t.Fatalf("EWMA = %v, want ~700", q)
	}
}

// An engine with requests queued is saturated even when it reports free
// lanes (device slots or KV are the real limit).
func TestScoreQueuedMeansSaturated(t *testing.T) {
	tr := NewTracker(DefaultWeights())
	tr.Set("a", &Load{Engine: "ninfer", Running: 2, Waiting: 0, Lanes: 5, LastUpdated: time.Now()})
	tr.Set("b", &Load{Engine: "ninfer", Running: 2, Waiting: 2, Lanes: 5, LastUpdated: time.Now()})
	if a, b := tr.Score("a"), tr.Score("b"); a > 0.4 || b < 0.75 {
		t.Fatalf("scores: free lanes %.2f, queued %.2f; want queued >= 0.75", a, b)
	}
}
