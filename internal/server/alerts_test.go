package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgateway/internal/config"
)

type alertLog struct {
	mu  sync.Mutex
	got []Alert
}

func (l *alertLog) add(a Alert) { l.mu.Lock(); l.got = append(l.got, a); l.mu.Unlock() }
func (l *alertLog) kinds() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var k []string
	for _, a := range l.got {
		s := a.Kind
		if a.Resolved {
			s += "(resolved)"
		}
		k = append(k, s)
	}
	return strings.Join(k, ",")
}

// A service that stops answering alerts once after down_after_seconds and
// resolves when it's back; restarts notify, and three in a row are a loop.
func TestAlertsServiceDownAndRestarts(t *testing.T) {
	var down atomic.Bool
	var uptime atomic.Int64
	uptime.Store(3600)
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/slots":
			fmt.Fprint(w, `{"max_concurrency":4,"requests_processing":0,"requests_waiting":0}`)
		case "/usage":
			fmt.Fprintf(w, `{"uptime":{"seconds":%d}}`, uptime.Load())
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"alert-model"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer eng.Close()
	u, _ := url.Parse(eng.URL)
	s := testServer(t, fmt.Sprintf(`{"gateway":{"port":0},"discovery":{"local_ports":[%s],"remote_ports":[]},
	  "alerts":{"down_after_seconds":1}}`, u.Port()), nil)
	log := &alertLog{}
	s.alerts.send = log.add

	s.PollOnce()
	s.CheckAlerts()
	if log.kinds() != "" {
		t.Fatalf("alerts while healthy: %s", log.kinds())
	}
	down.Store(true)
	s.PollOnce()
	s.PollOnce()    // two failed polls in a row mark it down
	s.CheckAlerts() // down, but not for long enough yet
	if log.kinds() != "" {
		t.Fatalf("alerted before down_after_seconds: %s", log.kinds())
	}
	time.Sleep(1100 * time.Millisecond)
	s.PollOnce()
	s.CheckAlerts()
	s.CheckAlerts() // still down: no repeat
	if log.kinds() != "service_down" {
		t.Fatalf("after 1s down: %s", log.kinds())
	}
	down.Store(false)
	uptime.Store(20) // it came back as a fresh process
	s.mu.Lock()
	clear(s.lastPollAt) // skip the 5 s re-probe spacing for down members
	s.mu.Unlock()
	s.PollOnce()
	s.CheckAlerts()
	if k := log.kinds(); k != "service_down,service_down(resolved),engine_restart" {
		t.Fatalf("after recovery: %s", k)
	}
	for i := 0; i < 2; i++ {
		uptime.Store(1000)
		s.PollOnce()
		s.CheckAlerts()
		uptime.Store(5)
		s.PollOnce()
		s.CheckAlerts()
	}
	if k := log.kinds(); !strings.HasSuffix(k, "engine_restart,engine_restart,restart_loop") {
		t.Fatalf("restart loop: %s", k)
	}
}

// A link agent the alerter remembers is not mistaken for a service.
func TestAlertsLinkNotAService(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	log := &alertLog{}
	s.alerts.send = log.add
	s.alerts.seen["link:ws6000"] = time.Now()
	s.CheckAlerts()
	if strings.Contains(log.kinds(), "service_down") {
		t.Fatalf("link agent reported as a down service: %s", log.kinds())
	}
}

// The first refused request of the day notifies, once.
func TestAlertsQuotaReached(t *testing.T) {
	s := testServer(t, `{"gateway":{"port":0}}`, nil)
	log := &alertLog{}
	s.alerts.send = log.add
	s.store.CreateKey("gina", "k", "user", false)
	s.store.SetDailyLimit("gina", 100)
	s.usage.Record("gina", "", "m", 90, 20)
	for i := 0; i < 3; i++ {
		s.enforceDailyLimit(httptest.NewRecorder(), "gina")
	}
	if log.kinds() != "quota_reached" {
		t.Fatalf("quota alerts: %s", log.kinds())
	}
}

// Each webhook format carries the title and message the way that service
// expects; min_severity filters.
func TestAlertWebhookFormats(t *testing.T) {
	type got struct {
		ct, title, prio, body string
	}
	ch := make(chan got, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{r.Header.Get("Content-Type"), r.Header.Get("Title"), r.Header.Get("Priority"), string(b)}
	}))
	defer hook.Close()
	a := Alert{Kind: "service_down", Severity: "critical", Title: "box :8000 is down", Message: "not answering"}
	for format, check := range map[string]func(got) bool{
		"ntfy": func(g got) bool { return g.title == a.Title && g.prio == "urgent" && g.body == a.Message },
		"slack": func(g got) bool {
			var m map[string]string
			json.Unmarshal([]byte(g.body), &m)
			return strings.Contains(m["text"], a.Title)
		},
		"discord": func(g got) bool {
			var m map[string]string
			json.Unmarshal([]byte(g.body), &m)
			return strings.Contains(m["content"], a.Message)
		},
		"json": func(g got) bool {
			var m map[string]any
			json.Unmarshal([]byte(g.body), &m)
			return m["kind"] == "service_down" && m["severity"] == "critical"
		},
	} {
		if err := postAlert(config.AlertWebhook{URL: hook.URL, Format: format}, a); err != nil {
			t.Fatal(err)
		}
		if g := <-ch; !check(g) {
			t.Fatalf("%s webhook got %+v", format, g)
		}
	}
	// deliver: an info alert doesn't reach a warning-and-up webhook.
	s := testServer(t, fmt.Sprintf(`{"gateway":{"port":0},"alerts":{"webhooks":[{"url":%q,"format":"json"}]}}`, hook.URL), nil)
	s.deliver(Alert{Kind: "quota_reached", Severity: "info", Title: "x", Message: "y"})
	s.deliver(Alert{Kind: "disk_low", Severity: "critical", Title: "disk", Message: "full"})
	select {
	case g := <-ch:
		if !strings.Contains(g.body, "disk_low") {
			t.Fatalf("first delivered = %s", g.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("critical alert not delivered")
	}
	select {
	case g := <-ch:
		t.Fatalf("info alert delivered to a warning webhook: %s", g.body)
	case <-time.After(300 * time.Millisecond):
	}
}
