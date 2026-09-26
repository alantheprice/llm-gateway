// First-run bootstrap (4b): when a fresh install has zero accounts, "/"
// serves a one-time create-admin form instead of a dead login. The form
// disappears the moment an account exists.
package server

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
)

// CountUsers: total PB accounts (0 on a fresh install).
func (s *Server) CountUsers() (int64, error) {
	return s.pb.CountUsers()
}

// handleBootstrapPage: GET → form; POST → create the admin, then redirect
// to login with a success note.
func (s *Server) handleBootstrapPage(w http.ResponseWriter, r *http.Request) {
	app := s.EmbeddedPB()
	if app == nil {
		errBody(w, 503, "bootstrap unavailable (embedded PocketBase not attached)")
		return
	}
	// Never render if an account snuck in since the count.
	if app == nil {
		return
	}
	if n, err := app.CountUsers(); err != nil || n > 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			errBody(w, 400, "bad form")
			return
		}
		user := strings.TrimSpace(r.FormValue("username"))
		pass := r.FormValue("password")
		pass2 := r.FormValue("password2")
		if user == "" || len(pass) < 8 {
			s.renderBootstrap(w, user, "Username required, password must be at least 8 characters.")
			return
		}
		if pass != pass2 {
			s.renderBootstrap(w, user, "Passwords do not match.")
			return
		}
		if err := app.CreateAdminUser(user, pass); err != nil {
			s.renderBootstrap(w, user, "Could not create the account: "+err.Error())
			return
		}
		// Dashboard superuser env too (parity with the CLI flow).
		_, _ = app.EnsureSuperuserEnv(pbDataDirForBootstrap(), "", "")
		s.renderBootstrapDone(w, user)
		return
	}
	s.renderBootstrap(w, "", "")
}

// pbDataDirForBootstrap: the PB data dir env, for the superuser env file.
func pbDataDirForBootstrap() string { return os.Getenv("PB_DATA_DIR") }

func renderBootstrapShell(w http.ResponseWriter, inner string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>Set up llm-gateway</title><style>`+
		`body{font:16px/1.6 -apple-system,Segoe UI,Roboto,sans-serif;background:#0d1117;color:#e6edf3;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}`+
		`.card{background:#161b22;border:1px solid #30363d;border-radius:10px;padding:2rem;width:360px}`+
		`h1{font-size:1.1rem;margin:0 0 .3rem}.sub{color:#8b949e;font-size:13px;margin:0 0 1.2rem}`+
		`label{display:block;font-size:13px;margin:.8rem 0 .3rem;color:#8b949e}`+
		`input{width:100%%;box-sizing:border-box;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e6edf3;padding:.55rem .7rem;font-size:14px}`+
		`button{width:100%%;margin-top:1.2rem;background:#238636;border:none;border-radius:6px;color:#fff;padding:.6rem;font-size:14px;font-weight:600;cursor:pointer}`+
		`.err{background:#da363322;border:1px solid #f85149;color:#f85149;border-radius:6px;padding:.6rem .8rem;font-size:13px;margin-bottom:1rem}`+
		`.ok{color:#3fb950;font-size:14px}`+
		`a{color:#58a6ff;text-decoration:none;font-size:13px}`+
		`</style></head><body><div class="card">`+inner+`</div></body></html>`)
}

func (s *Server) renderBootstrap(w http.ResponseWriter, user, errMsg string) {
	err := ""
	if errMsg != "" {
		err = `<div class="err">` + html.EscapeString(errMsg) + `</div>`
	}
	u := html.EscapeString(user)
	renderBootstrapShell(w, `
<h1>Welcome to llm-gateway</h1>
<p class="sub">First run — create the administrator account. This form only appears while there are no users.</p>
`+err+`
<form method="post" action="/bootstrap">
<label>Admin username</label>
<input name="username" value="`+u+`" autofocus autocomplete="username" required>
<label>Password (min 8 chars)</label>
<input name="password" type="password" autocomplete="new-password" required>
<label>Repeat password</label>
<input name="password2" type="password" autocomplete="new-password" required>
<button type="submit">Create admin</button>
</form>
<p style="margin-top:1rem"><a href="/guide">Setup guide →</a></p>`)
}

func (s *Server) renderBootstrapDone(w http.ResponseWriter, user string) {
	renderBootstrapShell(w, `
<h1>Administrator created</h1>
<p class="sub">Account <strong>`+html.EscapeString(user)+`</strong> is ready.</p>
<p class="ok">✓ Setup complete</p>
<p style="margin-top:1.4rem"><a class="ok" href="/login">Continue to login →</a></p>
<p><a href="/guide">Setup guide</a></p>`)
}
