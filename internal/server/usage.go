package server

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageStore tracks per-user/per-key/per-kind counters (SPEC §8) with the
// same JSON shape as the Python gateway's usage.json.
type UsageStore struct {
	mu    sync.Mutex
	path  string
	dirty bool
	last  time.Time

	Data UsageFile
}

type UsageFile struct {
	Users map[string]*UserUsage            `json:"users"`
	Daily map[string]map[string]*UserUsage `json:"daily"`
}

type UserUsage struct {
	Requests     int `json:"requests"`
	PromptTokens int `json:"prompt_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedTokens: prompt tokens served from KV reuse (no prefill
	// compute). Populated from prompt_tokens_details.cached_tokens when
	// the engine reports it; 0 for older records.
	CachedTokens int                   `json:"cached_tokens,omitempty"`
	Keys         map[string]*KindTally `json:"keys"`
	Kinds        map[string]*KindTally `json:"kinds"`
}

type KindTally struct {
	Requests     int `json:"requests"`
	PromptTokens int `json:"prompt_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens,omitempty"`
}

func NewUsageStore(path string) *UsageStore {
	u := &UsageStore{path: path, Data: UsageFile{
		Users: map[string]*UserUsage{},
		Daily: map[string]map[string]*UserUsage{},
	}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &u.Data)
		if u.Data.Users == nil {
			u.Data.Users = map[string]*UserUsage{}
		}
		if u.Data.Daily == nil {
			u.Data.Daily = map[string]map[string]*UserUsage{}
		}
	}
	return u
}

func kindFor(model string) string {
	m := lower(model)
	if contains(m, "embed") {
		return "embeddings"
	}
	if contains(m, "fim") {
		return "fim"
	}
	return "chat"
}

func (u *UsageStore) Record(user, keyID, model string, prompt, output int) {
	u.RecordDetailed(user, keyID, model, prompt, output, 0)
}

// RecordDetailed is Record with engine-reported cache reuse.
func (u *UsageStore) RecordDetailed(user, keyID, model string, prompt, output, cached int) {
	kind := kindFor(model)
	today := time.Now().Format("2006-01-02")
	u.mu.Lock()
	defer u.mu.Unlock()
	usr, ok := u.Data.Users[user]
	if !ok {
		usr = &UserUsage{Keys: map[string]*KindTally{}, Kinds: map[string]*KindTally{}}
		u.Data.Users[user] = usr
	}
	applyTally(usr, keyID, kind, prompt, output, cached)
	// Per-day buckets (history charts + daily quotas). Python parity:
	// local-date keys, full UserUsage shape.
	day := u.Data.Daily[today]
	if day == nil {
		day = map[string]*UserUsage{}
		u.Data.Daily[today] = day
	}
	du := day[user]
	if du == nil {
		du = &UserUsage{Keys: map[string]*KindTally{}, Kinds: map[string]*KindTally{}}
		day[user] = du
	}
	applyTally(du, keyID, kind, prompt, output, cached)
	u.dirty = true
	u.flushIfDue()
}

func applyTally(usr *UserUsage, keyID, kind string, prompt, output, cached int) {
	usr.Requests++
	usr.PromptTokens += prompt
	usr.OutputTokens += output
	if cached > 0 {
		usr.CachedTokens += cached
	}
	if usr.Kinds == nil {
		usr.Kinds = map[string]*KindTally{}
	}
	if keyID != "" {
		if usr.Keys == nil {
			usr.Keys = map[string]*KindTally{}
		}
		tally(usr.Keys, keyID, prompt, output, cached)
	}
	tally(usr.Kinds, kind, prompt, output, cached)
}

func tally(m map[string]*KindTally, name string, p, o, c int) {
	t, ok := m[name]
	if !ok {
		t = &KindTally{}
		m[name] = t
	}
	t.Requests++
	t.PromptTokens += p
	t.OutputTokens += o
	if c > 0 {
		t.CachedTokens += c
	}
}

const flushInterval = 60 * time.Second

func (u *UsageStore) flushIfDue() {
	if u.dirty && time.Since(u.last) >= flushInterval {
		u.flushLocked()
	}
}

func (u *UsageStore) Flush() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushLocked()
}

func (u *UsageStore) flushLocked() {
	if !u.dirty {
		return
	}
	data, err := json.MarshalIndent(u.Data, "", "  ")
	if err == nil {
		tmp := u.path + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			if os.Rename(tmp, u.path) == nil {
				u.dirty = false
				u.last = time.Now()
			}
		}
	}
}

func lower(s string) string { return strings.ToLower(s) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// KeyUsage snapshots the per-key tallies for a username (auth.ListKeys merge).
func (u *UsageStore) KeyUsage(username string) map[string]map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]map[string]int{}
	usr, ok := u.Data.Users[username]
	if !ok {
		return out
	}
	for id, t := range usr.Keys {
		out[id] = map[string]int{
			"requests": t.Requests, "prompt_tokens": t.PromptTokens, "output_tokens": t.OutputTokens,
		}
	}
	return out
}

// UsersSnapshot returns (all_time, today) per-user usage maps.
func (u *UsageStore) UsersSnapshot() (map[string]*UserUsage, map[string]*UserUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	all := make(map[string]*UserUsage, len(u.Data.Users))
	for k, v := range u.Data.Users {
		cp := *v
		all[k] = &cp
	}
	todays := u.Data.Daily[today]
	out := make(map[string]*UserUsage, len(todays))
	for k, v := range todays {
		cp := *v
		out[k] = &cp
	}
	return all, out
}

// TodaySnapshot returns today's per-user usage (copy).
func (u *UsageStore) TodaySnapshot() map[string]*UserUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	out := map[string]*UserUsage{}
	for k, v := range u.Data.Daily[today] {
		cp := *v
		out[k] = &cp
	}
	return out
}

// AvgDailyTokens returns the mean total tokens (prompt+output) per day over
// the last n recorded days (excluding today — today's partials would bias
// it down).
func (u *UsageStore) AvgDailyTokens(n int) float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n <= 0 {
		n = 7
	}
	today := time.Now().Format("2006-01-02")
	days := make([]string, 0, len(u.Data.Daily))
	for d := range u.Data.Daily {
		if d != today {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	if len(days) > n {
		days = days[len(days)-n:]
	}
	if len(days) == 0 {
		return 0
	}
	var total float64
	for _, d := range days {
		for _, t := range u.Data.Daily[d] {
			if t != nil {
				total += float64(t.PromptTokens + t.OutputTokens)
			}
		}
	}
	return total / float64(len(days))
}

// User returns the caller's usage record (nil if unknown).
func (u *UsageStore) User(username string) *UserUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Data.Users[username]
}

// TodayTokens returns the user's prompt+output tokens for the current UTC
// day (the quota window). In-memory counters are always current — Daily is
// written at Record time, not only at flush.
func (u *UsageStore) TodayTokens(username string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	t := u.Data.Daily[today][username]
	if t == nil {
		return 0
	}
	return t.PromptTokens + t.OutputTokens
}

// DailyTop-level access for history: returns sorted days (last 30) and the
// per-day token series — per-user for admins, own-only for users.
func (u *UsageStore) History(username string, isAdmin bool) ([]string, map[string]map[string]int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	days := make([]string, 0, len(u.Data.Daily))
	for d := range u.Data.Daily {
		days = append(days, d)
	}
	sort.Strings(days)
	if len(days) > 30 {
		days = days[len(days)-30:]
	}
	out := map[string]map[string]int{}
	for _, d := range days {
		perUser := u.Data.Daily[d]
		m := map[string]int{}
		if isAdmin {
			for usr, v := range perUser {
				m[usr] = v.PromptTokens + v.OutputTokens
			}
		} else if v, ok := perUser[username]; ok {
			m[username] = v.PromptTokens + v.OutputTokens
		}
		out[d] = m
	}
	return days, out
}
