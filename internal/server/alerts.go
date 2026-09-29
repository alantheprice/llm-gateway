package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"llmgateway/internal/config"
	"llmgateway/internal/link"
)

// Alerts: the gateway watches for problems and notifies webhooks (ntfy for
// phone push, Slack, Discord, or plain JSON). A problem fires once when it
// starts and once more when it clears.
//
//	service_down    a service (engine) down or unreachable for down_after_seconds
//	link_down       a GPU link agent (admin-owned, or serving a shared model) disconnected
//	engine_restart  an engine restarted (its uptime went back)
//	restart_loop    3+ restarts within 10 minutes
//	disk_low        the gateway's data disk nearly full
//	quota_reached   a user hit their daily token limit (once per user per day)
//
// Services and links seen in the last week are remembered, so one that
// vanishes (e.g. dropped by discovery after failing) still counts as down.

const (
	alertCheckEvery  = 30 * time.Second
	alertForgetAfter = 7 * 24 * time.Hour
	restartLoopN     = 3
	restartLoopIn    = 10 * time.Minute
	alertsKept       = 100
)

var severityRank = map[string]int{"info": 0, "warning": 1, "critical": 2}

// Alert is one notification (firing or resolved).
type Alert struct {
	Key      string    `json:"key"` // kind:subject — one firing alert per key
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"` // info | warning | critical
	Title    string    `json:"title"`
	Message  string    `json:"message"`
	Resolved bool      `json:"resolved,omitempty"`
	At       time.Time `json:"at"`
}

type alerter struct {
	mu        sync.Mutex
	firing    map[string]Alert
	recent    []Alert              // newest last
	seen      map[string]time.Time // service/link subject → last seen up
	downSince map[string]time.Time
	uptime    map[string]float64
	restarts  map[string][]time.Time
	quotaDay  map[string]string // user → UTC day already alerted
	dismissed map[string]bool   // subjects the admin stopped watching
	send      func(Alert)       // test hook; nil = webhooks
}

func newAlerter() *alerter {
	return &alerter{firing: map[string]Alert{}, seen: map[string]time.Time{}, downSince: map[string]time.Time{},
		uptime: map[string]float64{}, restarts: map[string][]time.Time{}, quotaDay: map[string]string{}, dismissed: map[string]bool{}}
}

// raise fires key unless it is already firing; resolve clears it.
func (s *Server) raise(a Alert) {
	s.alerts.mu.Lock()
	if _, on := s.alerts.firing[a.Key]; on {
		s.alerts.mu.Unlock()
		return
	}
	a.At = time.Now()
	s.alerts.firing[a.Key] = a
	s.alerts.record(a)
	s.alerts.mu.Unlock()
	s.deliver(a)
}

func (s *Server) clearAlert(key, message string) {
	s.alerts.mu.Lock()
	a, on := s.alerts.firing[key]
	if !on {
		s.alerts.mu.Unlock()
		return
	}
	delete(s.alerts.firing, key)
	a.Resolved, a.Message, a.At = true, message, time.Now()
	a.Title = "Resolved: " + a.Title
	a.Severity = "info"
	s.alerts.record(a)
	s.alerts.mu.Unlock()
	s.deliver(a)
}

// notify sends a one-off alert (no firing state).
func (s *Server) notify(a Alert) {
	a.At = time.Now()
	s.alerts.mu.Lock()
	s.alerts.record(a)
	s.alerts.mu.Unlock()
	s.deliver(a)
}

func (al *alerter) record(a Alert) {
	al.recent = append(al.recent, a)
	if len(al.recent) > alertsKept {
		al.recent = al.recent[len(al.recent)-alertsKept:]
	}
}

func (s *Server) alertsCfg() config.AlertsCfg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Alerts
}

func (s *Server) alertEnabled(kind string) bool {
	return !slices.Contains(s.alertsCfg().Disabled, kind)
}

// deliver logs the alert and posts it to every webhook that wants it.
func (s *Server) deliver(a Alert) {
	log.Printf("ALERT [%s] %s: %s", a.Severity, a.Title, a.Message)
	if s.alerts.send != nil {
		s.alerts.send(a)
		return
	}
	for _, wh := range s.alertsCfg().Webhooks {
		min := wh.MinSeverity
		if min == "" {
			min = "warning"
		}
		// Resolutions go wherever the alert went.
		sev := a.Severity
		if a.Resolved {
			sev = "critical"
		}
		if severityRank[sev] < severityRank[min] {
			continue
		}
		go func(wh config.AlertWebhook) {
			if err := postAlert(wh, a); err != nil {
				log.Printf("alert webhook %s: %v", redactURL(wh.URL), err)
			}
		}(wh)
	}
}

func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			return u[:i+3] + rest[:j] + "/…"
		}
	}
	return u
}

// postAlert sends one alert in the webhook's format (one retry).
func postAlert(wh config.AlertWebhook, a Alert) error {
	var body []byte
	headers := map[string]string{"Content-Type": "application/json"}
	text := a.Title + "\n" + a.Message
	switch wh.Format {
	case "ntfy":
		body = []byte(a.Message)
		headers = map[string]string{"Content-Type": "text/plain; charset=utf-8", "Title": a.Title,
			"Priority": map[string]string{"info": "default", "warning": "high", "critical": "urgent"}[a.Severity],
			"Tags":     map[bool]string{true: "white_check_mark", false: map[string]string{"info": "information_source", "warning": "warning", "critical": "rotating_light"}[a.Severity]}[a.Resolved]}
	case "slack":
		body, _ = json.Marshal(map[string]string{"text": "*" + a.Title + "*\n" + a.Message})
	case "discord":
		body, _ = json.Marshal(map[string]string{"content": "**" + a.Title + "**\n" + a.Message})
	default:
		body, _ = json.Marshal(map[string]any{"kind": a.Kind, "severity": a.Severity, "title": a.Title,
			"message": a.Message, "resolved": a.Resolved, "at": a.At.UTC().Format(time.RFC3339), "text": text})
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		var resp *http.Response
		resp, err = http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		time.Sleep(2 * time.Second)
	}
	return err
}

// ---- checks ----

// CheckAlerts runs every check once (the main loop calls it every 30 s).
func (s *Server) CheckAlerts() {
	s.checkServices()
	s.checkLinks()
	s.checkRestarts()
	s.checkDisk()
}

// watchedServices: pool members and every engine the gateway polls.
func (s *Server) watchedServices() []string {
	set := map[string]bool{}
	s.mu.Lock()
	for _, p := range s.cfg.ModelPools {
		for _, m := range p.Members {
			if m.Backend != "" && !isLinkURL(m.Backend) {
				set[m.Backend] = true
			}
		}
	}
	for u := range s.backends {
		set[u] = true
	}
	s.mu.Unlock()
	for _, u := range s.linkReg.LiveURLs() {
		set[u] = true // a link's engines; the link itself is checked separately
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func (s *Server) checkServices() {
	after := time.Duration(s.alertsCfg().DownAfterSeconds) * time.Second
	if after <= 0 {
		after = 2 * time.Minute
	}
	now := time.Now()
	live := map[string]bool{}
	for _, u := range s.watchedServices() {
		live[u] = true
	}
	s.alerts.mu.Lock()
	subjects := map[string]bool{}
	for u := range live {
		subjects[u] = true
	}
	for u, at := range s.alerts.seen {
		if now.Sub(at) > alertForgetAfter {
			delete(s.alerts.seen, u)
			continue
		}
		subjects[u] = true
	}
	s.alerts.mu.Unlock()

	for u := range subjects {
		s.alerts.mu.Lock()
		dismissed := s.alerts.dismissed[u]
		s.alerts.mu.Unlock()
		if dismissed {
			continue
		}
		// A link engine whose agent is gone is covered by link_down.
		if isLinkURL(u) && !live[u] {
			continue
		}
		down := !live[u] || s.tracker.IsDown(u)
		key := "service_down:" + u
		name := s.gpuIdentity(u).Label
		s.alerts.mu.Lock()
		if !down {
			s.alerts.seen[u] = now
			delete(s.alerts.downSince, u)
			s.alerts.mu.Unlock()
			s.clearAlert(key, name+" is answering again.")
			continue
		}
		since, ok := s.alerts.downSince[u]
		if !ok {
			since = now
			s.alerts.downSince[u] = now
		}
		s.alerts.mu.Unlock()
		if now.Sub(since) >= after && s.alertEnabled("service_down") {
			why := "isn't answering"
			if !live[u] {
				why = "is gone from discovery (not answering)"
			}
			s.raise(Alert{Key: key, Kind: "service_down", Severity: "critical",
				Title: name + " is down",
				Message: fmt.Sprintf("%s (%s) %s since %s. Requests for its models go to other services, or fail if there are none.",
					name, strings.Join(s.modelsOf(u), ", "), why, since.Format("15:04 MST"))})
		}
	}
}

// watchedLinks: link agents that are platform capacity — owned by an
// admin, or serving a shared model.
func (s *Server) watchedLink(c *link.Conn) bool {
	if c.Owner == "" || s.store.RoleOf(c.Owner) == "admin" {
		return true
	}
	for _, e := range c.Engines() {
		u := link.VirtualURL(c.Agent, e.Port)
		if s.servesAnyPool(u, s.platformModelsFor(u)) {
			return true
		}
	}
	return false
}

func (s *Server) checkLinks() {
	after := time.Duration(s.alertsCfg().DownAfterSeconds) * time.Second
	if after <= 0 {
		after = 2 * time.Minute
	}
	now := time.Now()
	connected := map[string]bool{}
	for _, c := range s.linkReg.Conns() {
		if s.watchedLink(c) {
			connected["link:"+c.Agent] = true
		}
	}
	s.alerts.mu.Lock()
	var subjects []string
	for k := range connected {
		s.alerts.seen[k] = now
	}
	for k, at := range s.alerts.seen {
		if strings.HasPrefix(k, "link:") && !s.alerts.dismissed[k] {
			if now.Sub(at) > alertForgetAfter {
				delete(s.alerts.seen, k)
				continue
			}
			subjects = append(subjects, k)
		}
	}
	s.alerts.mu.Unlock()
	for _, k := range subjects {
		agent := strings.TrimPrefix(k, "link:")
		key := "link_down:" + agent
		if connected[k] {
			s.clearAlert(key, "The link agent "+agent+" reconnected.")
			continue
		}
		s.alerts.mu.Lock()
		last := s.alerts.seen[k]
		s.alerts.mu.Unlock()
		if now.Sub(last) >= after && s.alertEnabled("link_down") {
			s.raise(Alert{Key: key, Kind: "link_down", Severity: "critical",
				Title:   "GPU link " + agent + " disconnected",
				Message: "The link agent on " + agent + " hasn't been connected since " + last.Format("15:04 MST") + ". Its GPUs get no traffic. On that machine: systemctl --user status llm-link-agent (Linux)."})
		}
	}
}

// checkRestarts: an engine whose reported uptime went back restarted.
func (s *Server) checkRestarts() {
	s.mu.Lock()
	up := map[string]float64{}
	for u, raw := range s.lastMetrics {
		if v := usageNum(raw, "uptime", "seconds"); v > 0 {
			up[u] = v
		}
	}
	s.mu.Unlock()
	now := time.Now()
	for u, v := range up {
		s.alerts.mu.Lock()
		prev, known := s.alerts.uptime[u]
		s.alerts.uptime[u] = v
		restarted := known && v+5 < prev
		var n int
		if restarted {
			rs := append(s.alerts.restarts[u], now)
			for len(rs) > 0 && now.Sub(rs[0]) > restartLoopIn {
				rs = rs[1:]
			}
			s.alerts.restarts[u] = rs
			n = len(rs)
		}
		s.alerts.mu.Unlock()
		name := s.gpuIdentity(u).Label
		if restarted && s.alertEnabled("engine_restart") {
			s.notify(Alert{Key: "engine_restart:" + u, Kind: "engine_restart", Severity: "warning",
				Title:   name + " restarted",
				Message: fmt.Sprintf("%s restarted (up %s now, was up %s). Requests in progress on it were cut off.", name, fmtDur(v), fmtDur(prev))})
		}
		if n >= restartLoopN && s.alertEnabled("restart_loop") {
			s.raise(Alert{Key: "restart_loop:" + u, Kind: "restart_loop", Severity: "critical",
				Title:   name + " is restarting repeatedly",
				Message: fmt.Sprintf("%s restarted %d times in %s. Check its logs; a GPU fault can need a reboot.", name, n, restartLoopIn)})
		} else if !restarted && known && now.Sub(lastOr(s, u)) > restartLoopIn {
			s.clearAlert("restart_loop:"+u, name+" has stayed up for "+restartLoopIn.String()+".")
		}
	}
}

func lastOr(s *Server, u string) time.Time {
	s.alerts.mu.Lock()
	defer s.alerts.mu.Unlock()
	rs := s.alerts.restarts[u]
	if len(rs) == 0 {
		return time.Time{}
	}
	return rs[len(rs)-1]
}

func fmtDur(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(sec))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

// checkDisk: the disks holding the gateway's data.
func (s *Server) checkDisk() {
	s.mu.Lock()
	dirs := []string{filepath.Dir(s.cfg.Gateway.UsersFile), filepath.Dir(UsagePath(s.cfg))}
	s.mu.Unlock()
	dirs = append(dirs, s.dataDirs...)
	seenDev := map[string]bool{}
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(d, &st) != nil {
			continue
		}
		dev := fmt.Sprint(st.Blocks, "/", st.Bsize) // same filesystem, same size
		if seenDev[dev] {
			continue
		}
		seenDev[dev] = true
		free := float64(st.Bavail) * float64(st.Bsize)
		total := float64(st.Blocks) * float64(st.Bsize)
		if total <= 0 {
			continue
		}
		pct := 100 * free / total
		key := "disk_low:" + d
		switch {
		case (pct < 5 || free < 2<<30) && s.alertEnabled("disk_low"):
			s.raise(Alert{Key: key, Kind: "disk_low", Severity: "critical",
				Title:   "Disk nearly full",
				Message: fmt.Sprintf("%s has %.1f GB free (%.1f%%). The gateway's database and usage records live there; when it fills, history stops being saved.", d, free/(1<<30), pct)})
		case pct > 8 && free > 4<<30:
			s.clearAlert(key, fmt.Sprintf("%s has %.1f GB free again.", d, free/(1<<30)))
		}
	}
}

// quotaReached: called when a request is refused for the daily limit.
func (s *Server) quotaReached(user string, limit int) {
	day := time.Now().UTC().Format("2006-01-02")
	s.alerts.mu.Lock()
	already := s.alerts.quotaDay[user] == day
	s.alerts.quotaDay[user] = day
	s.alerts.mu.Unlock()
	if already || !s.alertEnabled("quota_reached") {
		return
	}
	s.notify(Alert{Key: "quota_reached:" + user, Kind: "quota_reached", Severity: "info",
		Title:   user + " reached their daily limit",
		Message: fmt.Sprintf("%s used their %d-token daily limit; their requests are refused until 00:00 UTC. Raise it in Users if that's not intended.", user, limit)})
}

// ---- admin API ----

// handleAPIAlerts: GET the firing and recent alerts; POST {action: test |
// dismiss, key}.
func (s *Server) handleAPIAlerts(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		errBody(w, http.StatusForbidden, "admin only")
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Action string `json:"action"`
			Key    string `json:"key"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		switch body.Action {
		case "test":
			if len(s.alertsCfg().Webhooks) == 0 {
				errBody(w, http.StatusBadRequest, "no webhooks configured")
				return
			}
			var errs []string
			for _, wh := range s.alertsCfg().Webhooks {
				if err := postAlert(wh, Alert{Kind: "test", Severity: "critical", Title: "Test alert from LLM Gateway",
					Message: "If you can read this, alerts reach you here.", At: time.Now()}); err != nil {
					errs = append(errs, redactURL(wh.URL)+": "+err.Error())
				}
			}
			if len(errs) > 0 {
				errBody(w, http.StatusBadGateway, strings.Join(errs, "; "))
				return
			}
		case "dismiss":
			// Stop watching the subject (a service or link removed for good).
			subject := body.Key
			if i := strings.Index(subject, ":"); i >= 0 {
				subject = subject[i+1:]
			}
			if strings.HasPrefix(body.Key, "link_down:") {
				subject = "link:" + subject
			}
			s.alerts.mu.Lock()
			s.alerts.dismissed[subject] = true
			delete(s.alerts.seen, subject)
			delete(s.alerts.firing, body.Key)
			s.alerts.mu.Unlock()
		default:
			errBody(w, http.StatusBadRequest, "unknown action")
			return
		}
	}
	s.alerts.mu.Lock()
	firing := make([]Alert, 0, len(s.alerts.firing))
	for _, a := range s.alerts.firing {
		firing = append(firing, a)
	}
	recent := slices.Clone(s.alerts.recent)
	s.alerts.mu.Unlock()
	sort.Slice(firing, func(i, j int) bool { return firing[i].At.Before(firing[j].At) })
	slices.Reverse(recent)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"firing": firing, "recent": recent, "webhooks": len(s.alertsCfg().Webhooks)})
}
