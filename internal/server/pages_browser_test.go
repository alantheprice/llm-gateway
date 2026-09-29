package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/embeddedpb"
)

// Every page, rendered by a real headless browser against a running
// gateway with live-looking data: no uncaught script error, no section left
// on "Loading…", no bounce to the login page. (The Costs page once shipped
// writing to an element that didn't exist, and everything below it stayed
// on "Loading…" for days.)
//
// Needs Chrome/Chromium; skipped without one unless REQUIRE_BROWSER=1 (CI).

func findBrowser() string {
	for _, b := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
}

// pageEngine: an engine with load, usage, energy and a model card.
func pageEngine(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/slots":
			fmt.Fprint(w, `{"max_concurrency":8,"requests_processing":2,"requests_waiting":0}`)
		case "/usage":
			fmt.Fprint(w, `{"model":"page-model","uptime":{"seconds":3600},
			  "energy":{"today":{"kwh":1.5,"cost_usd":0.19,"tokens":100000},"rolling_30d":{"kwh":30,"cost_usd":3.75},
			    "daily":{"2026-09-28":{"kwh":2,"cost_usd":0.25,"tokens":200000}}},
			  "tokens":{"input":{"total":500000,"cache_hits":300000,"cache_hit_rate_pct":60},"output":{"total":20000}},
			  "throughput":{"decode_tok_per_s":80,"prefill_tok_per_s":4000},"lanes":{"capacity":8,"processing":2},
			  "health":{"kv_pressure_spills":0,"sessions_evicted":0}}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"page-model","max_model_len":131072}]}`)
		case "/v1/models/page-model":
			fmt.Fprint(w, `{"id":"page-model","model_card":{"schema":"ninfer-model-card/1","name":"Page Model",
			  "hardware":{"gpu":"RTX 5090","vram_mib":32607},"weights":{"format":"nvfp4"},"serving":{"max_context":131072}}}`)
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// consoleErr: Chrome's stderr console lines for uncaught errors.
var consoleErr = regexp.MustCompile(`CONSOLE[^"]*"(Uncaught[^"]*)"`)

func TestPagesRenderInBrowser(t *testing.T) {
	browser := findBrowser()
	if browser == "" {
		if os.Getenv("REQUIRE_BROWSER") != "" {
			t.Fatal("REQUIRE_BROWSER set but no Chrome/Chromium found")
		}
		t.Skip("no Chrome/Chromium; set REQUIRE_BROWSER=1 to require")
	}
	if err := InitUI(); err != nil {
		t.Fatal(err)
	}
	eng := pageEngine(t)
	u, _ := url.Parse(eng.URL)
	conf := fmt.Sprintf(`{"gateway":{"port":0},"discovery":{"local_ports":[%s],"remote_ports":[]},
	  "model_pools":{"page-pool":{"members":[{"backend":%q,"model_id":"page-model"}]}},
	  "hosts":[{"label":"box (5090)","ips":["127.0.0.1"],"overhead_watts":100,"hardware_cost_usd":4000,"purchased":"2026-06-01","amortize_years":3,"gpus":[{"name":"RTX 5090"}]}],
	  "price_book":{"prompt_usd_per_m":0.1,"cached_usd_per_m":0.01,"output_usd_per_m":0.3}}`, u.Port(), eng.URL)
	// A real embedded PocketBase (accounts, analytics, ops tables) on a free
	// port, so the account, users and analytics pages load real data.
	pbDir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pbPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	app, err := embeddedpb.Start(embeddedpb.Config{DataDir: pbDir, Port: pbPort})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.WaitUntilHealthy("127.0.0.1", pbPort, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := app.InitOpsTables(); err != nil {
		t.Fatal(err)
	}
	if err := app.InitRequestsSchema(); err != nil {
		t.Fatal(err)
	}
	suPass, err := app.EnsureSuperuserEnv(pbDir, "admin@llm.local", "page-test-superuser")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"admin", "carol"} {
		if err := app.CreateAdminUser(u, "page-test-password"); err != nil {
			t.Fatal(err)
		}
	}
	old := os.Getenv("POCKETBASE_URL")
	os.Setenv("POCKETBASE_URL", fmt.Sprintf("http://127.0.0.1:%d", pbPort))
	t.Cleanup(func() { os.Setenv("POCKETBASE_URL", old) })

	s := testServer(t, conf, nil)
	s.pb.SetSuperuser("admin@llm.local", suPass)
	s.SetOps(app)
	s.SetEmbeddedPB(app)
	s.PollOnce()
	s.usage.Record("admin", "", "page-model", 1000, 200)
	s.usage.Record("carol", "", "page-model", 500, 100)
	s.store.CreateKey("admin", "k", "admin", false)
	s.store.CreateKey("carol", "k", "user", false)

	h := s.Handler()
	for _, role := range []string{"admin", "user"} {
		user := map[string]string{"admin": "admin", "user": "carol"}[role]
		tok := s.store.SignSession(auth.Claims{U: user, Role: role}, time.Hour)
		pages := []string{"/chat", "/gpus", "/models", "/images", "/keys", "/account", "/usage/me", "/guide/mcp"}
		if role == "admin" {
			pages = append(pages, "/admin/users/page", "/admin/system", "/admin/costs",
				"/admin/analytics/page", "/admin/config/page", "/admin/routing")
		}
		for _, p := range pages {
			p := p
			t.Run(role+p, func(t *testing.T) {
				t.Parallel()
				checkPage(t, browser, h, tok, p)
			})
		}
	}
}

// get: one in-process request as the signed-in browser would make it.
func get(h http.Handler, tok, method, path string) (int, string, []byte) {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()
}

var assetRef = regexp.MustCompile(`(src|href)="/static/([^"?]+)(\?[^"]*)?"`)

// pageShim replaces fetch: answers from the recorded responses and notes
// every request it couldn't answer in data-miss (the test fetches those
// in-process and reloads). Pages run from file://, so the browser needs no
// network at all.
const pageShim = `<script>
(function () {
  var R = __RESPONSES__, miss = [];
  window.fetch = function (u, o) {
    var m = ((o && o.method) || 'GET').toUpperCase();
    var x = new URL(String(u), 'http://gw.test/');
    var key = m + ' ' + x.pathname + x.search;
    var r = R[key];
    if (!r) {
      miss.push(key);
      document.documentElement.setAttribute('data-miss', JSON.stringify(miss));
      r = {status: 200, ct: 'application/json', body: '{}'};
    }
    return Promise.resolve(new Response(r.body, {status: r.status, headers: {'Content-Type': r.ct}}));
  };
  if (navigator.serviceWorker) navigator.serviceWorker.register = function () { return Promise.resolve(); };
})();
</script>`

type recorded struct {
	Status int    `json:"status"`
	CT     string `json:"ct"`
	Body   string `json:"body"`
}

var scriptBlock = regexp.MustCompile(`(?s)<script\b.*?</script>`)

var missAttr = regexp.MustCompile(`data-miss="([^"]*)"`)

func checkPage(t *testing.T, browser string, s http.Handler, tok, path string) {
	dir := t.TempDir()
	code, _, page := get(s, tok, "GET", path)
	if code != 200 {
		t.Fatalf("GET %s = %d", path, code)
	}
	// Static assets next to the page file.
	os.MkdirAll(filepath.Join(dir, "static"), 0o755)
	html := assetRef.ReplaceAllStringFunc(string(page), func(m string) string {
		sm := assetRef.FindStringSubmatch(m)
		if _, _, body := get(s, tok, "GET", "/static/"+sm[2]); len(body) > 0 {
			os.WriteFile(filepath.Join(dir, "static", sm[2]), body, 0o644)
		}
		return sm[1] + `="static/` + sm[2] + `"`
	})
	responses := map[string]recorded{}
	for round := 0; round < 6; round++ {
		rj, _ := json.Marshal(responses)
		shim := strings.Replace(pageShim, "__RESPONSES__", string(rj), 1)
		doc := strings.Replace(html, "<head>", "<head>"+shim, 1)
		file := filepath.Join(dir, "page.html")
		os.WriteFile(file, []byte(doc), 0o644)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, browser, "--headless=new", "--no-sandbox", "--disable-gpu",
			"--disable-dev-shm-usage", "--no-first-run", "--user-data-dir="+filepath.Join(dir, "profile"),
			"--allow-file-access-from-files", "--enable-logging=stderr", "--v=0", "--virtual-time-budget=5000",
			"--dump-dom", "file://"+file)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		dom, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatalf("browser: %v\n%s", err, stderr.String())
		}
		out := string(dom)

		// Requests the page made that we haven't answered yet: answer them
		// in-process and render again.
		var missing []string
		if m := missAttr.FindStringSubmatch(out); m != nil {
			json.Unmarshal([]byte(strings.ReplaceAll(m[1], "&quot;", `"`)), &missing)
		}
		if len(missing) > 0 {
			for _, key := range missing {
				method, target, _ := strings.Cut(key, " ")
				code, ct, body := get(s, tok, method, target)
				responses[key] = recorded{Status: code, CT: ct, Body: string(body)}
			}
			continue
		}

		if m := consoleErr.FindAllStringSubmatch(stderr.String(), -1); len(m) > 0 {
			var errs []string
			for _, x := range m {
				errs = append(errs, x[1])
			}
			t.Errorf("script errors:\n  %s", strings.Join(errs, "\n  "))
		}
		for key, r := range responses {
			if r.Status >= 400 {
				t.Errorf("page request %s answered %d: %.200s", key, r.Status, r.Body)
			}
		}
		visible := scriptBlock.ReplaceAllString(out, "")
		if n := strings.Count(visible, "Loading…"); n > 0 {
			i := strings.Index(visible, "Loading…")
			t.Errorf("%d section(s) still say Loading…, first near: …%s…", n, visible[max(0, i-200):i])
		}
		return
	}
	t.Fatalf("page kept making new requests after 6 rounds")
}
