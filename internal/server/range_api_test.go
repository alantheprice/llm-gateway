package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/embeddedpb"
)

// Analytics and cost history answer for an explicit range of UTC days.
func TestAnalyticsAndCostRanges(t *testing.T) {
	app, _, _ := testPB(t)
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	s.SetOps(app)
	h := s.Handler()
	user := fmt.Sprintf("range%d", time.Now().UnixNano()%1e6) // shared DB: unique rows
	now := time.Now().UTC()
	for i, at := range []time.Time{now, now.AddDate(0, 0, -3), now.AddDate(0, 0, -3)} {
		if err := app.InsertRequests([]embeddedpb.RequestRecord{{TS: at, User: user, Backend: "http://b:1", Kind: "chat",
			Status: 200, Prompt: int64(100 * (i + 1)), Output: 10}}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) map[string]any {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.store.SignSession(auth.Claims{U: "root", Role: "admin"}, time.Hour)})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	requestsFor := func(m map[string]any) float64 {
		for _, u := range m["per_user"].([]any) {
			if u := u.(map[string]any); u["user"] == user {
				return u["requests"].(float64)
			}
		}
		return 0
	}
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	if n := requestsFor(get("/api/analytics?from=" + day(now) + "&to=" + day(now))); n != 1 {
		t.Fatalf("today = %v requests", n)
	}
	threeAgo := day(now.AddDate(0, 0, -3))
	if n := requestsFor(get("/api/analytics?from=" + threeAgo + "&to=" + threeAgo)); n != 2 {
		t.Fatalf("3 days ago = %v requests", n)
	}
	if n := requestsFor(get("/api/analytics?from=" + threeAgo + "&to=" + day(now))); n != 3 {
		t.Fatalf("both = %v requests", n)
	}

	// Cost history by range.
	d1, d2 := "2001-02-03", "2001-02-05"
	app.UpsertCostDay(d1, 1, 0.5, 2, 4, 1000)
	app.UpsertCostDay(d2, 1, 0.5, 2, 1, 1000)
	m := get("/api/costs/history?from=2001-02-01&to=2001-02-04")
	if days := m["days"].([]any); len(days) != 1 || days[0].(map[string]any)["day"] != d1 {
		t.Fatalf("cost range days = %v", days)
	}
	if tot := m["totals"].(map[string]any); tot["cost_usd"] != 3.5 || tot["value_usd"] != 4.0 {
		t.Fatalf("cost totals = %v", tot)
	}
}
