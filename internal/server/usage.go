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
	today := time.Now().UTC().Format("2006-01-02")
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
	// Merge the on-disk file's tallies into our map before writing: the
	// in-memory map only holds users seen since boot, and a blind
	// overwrite erased historical users on every restart (the "where did
	// my per-key usage go" bug). MAX-merge keeps both sides' maxima —
	// counters are monotone so this is exact.
	if data, err := os.ReadFile(u.path); err == nil {
		var disk UsageFile
		if json.Unmarshal(data, &disk) == nil {
			mergeUserUsage := func(mu, du *UserUsage) {
				if du == nil || mu == nil || du == mu {
					return
				}
				if du.Requests > mu.Requests {
					mu.Requests = du.Requests
				}
				if du.PromptTokens > mu.PromptTokens {
					mu.PromptTokens = du.PromptTokens
				}
				if du.OutputTokens > mu.OutputTokens {
					mu.OutputTokens = du.OutputTokens
				}
				if du.CachedTokens > mu.CachedTokens {
					mu.CachedTokens = du.CachedTokens
				}
			}
			for name, du := range disk.Users {
				if du == nil {
					continue
				}
				mu, ok := u.Data.Users[name]
				if !ok {
					u.Data.Users[name] = du
					continue
				}
				mergeUserUsage(mu, du)
			}
			for day, users := range disk.Daily {
				dm := u.Data.Daily[day]
				if dm == nil {
					u.Data.Daily[day] = users
					continue
				}
				for name, du := range users {
					mergeUserUsage(dm[name], du)
				}
			}
		}
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

// OpsRow: one flattened tally for the SQLite ops mirror.
type OpsRow struct {
	Day, User, Kind string
	Requests        int
	Prompt, Cached  int
	Output          int
}

// OpsRows flattens every tally the store holds: per-day per-user per-kind,
// per-day per-user per-key (kind = "key:<id>"), and lifetime per-user
// per-key (day = "lifetime"). MAX()-merge on the SQLite side makes this
// fully idempotent.
func (u *UsageStore) OpsRows() []OpsRow {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]OpsRow, 0, 256)
	add := func(day, user, kind string, t *KindTally) {
		if t == nil || (t.Requests == 0 && t.PromptTokens == 0 && t.OutputTokens == 0) {
			return
		}
		out = append(out, OpsRow{Day: day, User: user, Kind: kind,
			Requests: t.Requests, Prompt: t.PromptTokens, Cached: t.CachedTokens, Output: t.OutputTokens})
	}
	today := time.Now().UTC().Format("2006-01-02")
	// Lifetime per user: kinds + per-key.
	for user, usr := range u.Data.Users {
		if usr == nil {
			continue
		}
		for kind, kt := range usr.Kinds {
			add("lifetime", user, kind, kt)
		}
		for kid, kt := range usr.Keys {
			add("lifetime", user, "key:"+kid, kt)
		}
	}
	// Per-day per-user: kinds + per-key.
	for day, users := range u.Data.Daily {
		for user, usr := range users {
			if usr == nil {
				continue
			}
			for kind, kt := range usr.Kinds {
				add(day, user, kind, kt)
			}
			for kid, kt := range usr.Keys {
				add(day, user, "key:"+kid, kt)
			}
			_ = today
		}
	}
	return out
}

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
	today := time.Now().UTC().Format("2006-01-02")
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
	today := time.Now().UTC().Format("2006-01-02")
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
	today := time.Now().UTC().Format("2006-01-02")
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
	// Same local-date key RecordDetailed writes under (Python parity).
	today := time.Now().Format("2006-01-02")
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
