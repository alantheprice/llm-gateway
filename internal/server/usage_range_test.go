package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgateway/internal/auth"
)

func TestUsageRange(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0},"price_book":{"prompt_usd_per_m":1000,"cached_usd_per_m":100,"output_usd_per_m":2000}}`, nil)
	day := func(d string, user string, prompt, output int) {
		s.usage.mu.Lock()
		if s.usage.Data.Daily[d] == nil {
			s.usage.Data.Daily[d] = map[string]*UserUsage{}
		}
		s.usage.Data.Daily[d][user] = &UserUsage{Requests: 1, PromptTokens: prompt, OutputTokens: output,
			Kinds: map[string]*KindTally{"chat": {PromptTokens: prompt, OutputTokens: output}}}
		if s.usage.Data.Users[user] == nil {
			s.usage.Data.Users[user] = &UserUsage{}
		}
		lt := s.usage.Data.Users[user]
		lt.Requests++
		lt.PromptTokens += prompt
		lt.OutputTokens += output
		s.usage.mu.Unlock()
	}
	day("2026-09-01", "ann", 100, 10)
	day("2026-09-10", "ann", 200, 20)
	day("2026-09-10", "bob", 900, 90)
	day("2026-09-20", "bob", 50, 5)
	get := func(user, role, q string) map[string]any {
		r := httptest.NewRequest("GET", "/api/usage/range"+q, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: user, Role: role}, time.Hour)})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		if w.Code != 200 {
			t.Fatalf("%s: %d %v", q, w.Code, m)
		}
		return m
	}
	totals := func(m map[string]any) map[string]float64 {
		out := map[string]float64{}
		for _, u := range m["users"].([]any) {
			u := u.(map[string]any)
			out[u["user"].(string)] = u["total_tokens"].(float64)
		}
		return out
	}
	m := get("root", "admin", "?from=2026-09-05&to=2026-09-15")
	if got := totals(m); got["ann"] != 220 || got["bob"] != 990 || len(got) != 2 {
		t.Fatalf("range totals = %v", got)
	}
	if days := m["days"].([]any); len(days) != 1 || days[0] != "2026-09-10" {
		t.Fatalf("days = %v", days)
	}
	if u := m["users"].([]any)[0].(map[string]any); u["user"] != "bob" {
		t.Fatalf("not sorted by tokens: %v", u)
	}
	// Value at the price book: bob's 900 prompt + 90 output = $0.90 + $0.18.
	if u := m["users"].([]any)[0].(map[string]any); u["value_usd"] != 1.08 || m["price_book"] == nil {
		t.Fatalf("value = %v, book = %v", u["value_usd"], m["price_book"])
	}
	m = get("root", "admin", "")
	if got := totals(m); got["ann"] != 330 || got["bob"] != 1045 || m["first_day"] != "2026-09-01" {
		t.Fatalf("all time = %v first=%v", got, m["first_day"])
	}
	if s := m["series"].(map[string]any)["bob"].([]any); len(s) != 3 || s[1] != 990.0 {
		t.Fatalf("series = %v", s)
	}
	// A regular user sees only themselves.
	if got := totals(get("ann", "user", "?from=2026-09-01&to=2026-09-30")); len(got) != 1 || got["ann"] != 330 {
		t.Fatalf("user view = %v", got)
	}
}
