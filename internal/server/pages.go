package server

import (
	"net/http"
	"os"
	"strings"

	"llmgateway/internal/auth"
	"llmgateway/internal/web"
)

// lucideNav is the server-side SVG map.
var lucideNav = map[string]string{
	"message-square":   `<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>`,
	"route":            `<circle cx="6" cy="19" r="3"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="3"/>`,
	"code":             `<polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/>`,
	"line-chart":       `<path d="M3 3v18h18"/><path d="m19 9-5 5-4-4-3 3"/>`,
	"settings":         `<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>`,
	"book-open":        `<path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z"/><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z"/>`,
	"layout-dashboard": `<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>`,
	"gauge":            `<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>`,
	"scatter-chart":    `<circle cx="7.5" cy="7.5" r=".5" fill="currentColor"/><circle cx="18.5" cy="5.5" r=".5" fill="currentColor"/><circle cx="11.5" cy="11.5" r=".5" fill="currentColor"/><circle cx="7.5" cy="16.5" r=".5" fill="currentColor"/><circle cx="17.5" cy="14.5" r=".5" fill="currentColor"/><path d="M3 3v16a2 2 0 0 0 2 2h16"/>`,
	"image":            `<rect width="18" height="18" x="3" y="3" rx="2" ry="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/>`,
	"box":              `<path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z"/><path d="m3.3 7 8.7 5 8.7-5"/><path d="M12 22V12"/>`,
	"cpu":              `<rect width="16" height="16" x="4" y="4" rx="2"/><rect width="6" height="6" x="9" y="9" rx="1"/><path d="M15 2v2"/><path d="M15 20v2"/><path d="M2 15h2"/><path d="M2 9h2"/><path d="M20 15h2"/><path d="M20 9h2"/><path d="M9 2v2"/><path d="M9 20v2"/>`,
	"key-round":        `<path d="M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z"/><circle cx="16.5" cy="7.5" r=".5" fill="currentColor"/>`,
	"bar-chart-3":      `<path d="M3 3v18h18"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>`,
	"users":            `<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>`,
	"activity":         `<path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/>`,
	"log-out":          `<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>`,
	"shield":           `<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/>`,
	"sun":              `<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>`,
	"moon":             `<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>`,
	"send":             `<path d="M14.536 21.686a.5.5 0 0 0 .937-.024l6.5-19a.496.496 0 0 0-.635-.635l-19 6.5a.5.5 0 0 0-.024.937l7.93 3.18a2 2 0 0 1 1.112 1.11z"/><path d="m21.854 2.147-10.94 10.939"/>`,
	"plus":             `<path d="M5 12h14"/><path d="M12 5v14"/>`,
	"globe":            `<circle cx="12" cy="12" r="10"/><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"/><path d="M2 12h20"/>`,
	"menu":             `<line x1="4" x2="20" y1="6" y2="6"/><line x1="4" x2="20" y1="12" y2="12"/><line x1="4" x2="20" y1="18" y2="18"/>`,
	"plug":             `<path d="M12 22v-5"/><path d="M9 8V2"/><path d="M15 8V2"/><path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z"/>`,
	"sliders":          `<line x1="21" x2="14" y1="4" y2="4"/><line x1="10" x2="3" y1="4" y2="4"/><line x1="21" x2="12" y1="12" y2="12"/><line x1="8" x2="3" y1="12" y2="12"/><line x1="21" x2="16" y1="20" y2="20"/><line x1="12" x2="3" y1="20" y2="20"/><line x1="14" x2="14" y1="2" y2="6"/><line x1="8" x2="8" y1="10" y2="14"/><line x1="16" x2="16" y1="18" y2="22"/>`,
	"wallet":           `<path d="M21 12V7H5a2 2 0 0 1 0-4h14v4"/><path d="M3 5v14a2 2 0 0 0 2 2h16v-5"/><path d="M18 12a2 2 0 0 0 0 4h4v-4Z"/>`,
	"file-text":        `<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/>`,
	"brain":            `<path d="M12 5a3 3 0 1 0-5.997.125 4 4 0 0 0-2.526 5.77 4 4 0 0 0 .556 6.588A4 4 0 1 0 12 18Z"/><path d="M12 5a3 3 0 1 1 5.997.125 4 4 0 0 1 2.526 5.77 4 4 0 0 1-.556 6.588A4 4 0 1 1 12 18Z"/>`,
}

func iconSVG(name string) string {
	path := lucideNav[name]
	if path == "" {
		return ""
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" ` +
		`viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" ` +
		`stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`
}

// staticVer: max mtime of the source static dir, falling back
// to binary build time (embedded FS has no useful mtimes).
var staticVer = "1"

func computeStaticVer() {
	// Embedded FS assets have no useful mtimes; use the binary's build time.
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			staticVer = st.ModTime().UTC().Format("20060102150405")
		}
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, page, nav, title string) {
	sess, _ := s.sessionFrom(r)
	username := ""
	role := "user"
	if sess.U != "" {
		username = sess.U
		role = sess.Role
	}
	avatar := "?"
	if username != "" {
		avatar = strings.ToUpper(username[:1])
	}
	data := web.PageData{
		Nav: nav, Username: username, Role: role, Title: title,
		Error: r.URL.Query().Get("error"), Avatar: avatar,
		StaticVer: staticVer, Chrome: true,
	}
	if username != "" {
		// The theme is applied before the page paints (no flash).
		data.Extra = map[string]any{"theme": s.store.PrefsOf(username).Theme}
	}
	if page == "login.html" {
		data.Chrome = false
		data.Error = r.FormValue("error")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := web.Render(w, page, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) requireSessionPage(w http.ResponseWriter, r *http.Request) (sess sessionClaims, ok bool) {
	claims, valid := s.sessionFrom(r)
	if !valid {
		http.Redirect(w, r, "/", http.StatusFound)
		return claims, false
	}
	if claims.MCP {
		http.Redirect(w, r, "/change-password", http.StatusFound)
		return claims, false
	}
	return claims, true
}

type sessionClaims = auth.Claims

// InitUI parses templates + computes the static cache-bust version.
func InitUI() error {
	computeStaticVer()
	staticVerValue.Store(staticVer)
	return web.Load()
}

// ---- concrete page handlers ----

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFrom(r); ok {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "login.html", "", "Sign in")
}

func (s *Server) handleChangePWPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !sess.MCP {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "change_pw.html", "chat", "Set your password")
}

func (s *Server) handleChatPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "chat.html", "chat", "Chat")
}

func (s *Server) handleKeysPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "keys.html", "keys", "API Keys")
}

// handleAccountPage: /account moved into Settings.
func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings#account", http.StatusMovedPermanently)
}

func (s *Server) handleMyUsagePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "usage_me.html", "myusage", "Usage")
}

func (s *Server) handleAdminUsersPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "admin_users.html", "admin_users", "Users")
}

func (s *Server) handleAdminSystemPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok || sess.Role != "admin" {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	s.renderPage(w, r, "admin_system.html", "admin_system", "System")
}
