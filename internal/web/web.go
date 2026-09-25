// Package web embeds the UI (templates + static assets) and renders pages.
// Single binary: go:embed pulls everything in at build time.
package web

import (
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// PageData is the chrome context every template receives (Python render()
// parity: nav, username, role + extras).
type PageData struct {
	Nav       string
	Username  string
	Role      string
	Title     string
	Error     string
	Avatar    string
	StaticVer string
	Chrome    bool
	Extra     map[string]any
}

var templates atomic.Value // *template.Template

var iconFunc = func(name string) string { return "" }

// SetIconFunc wires the server's SVG icon renderer (avoids an import cycle:
// icons live in the server package's _LUCIDE_NAV port).
func SetIconFunc(f func(name string) string) { iconFunc = f }

// Load parses all templates (call once at startup; re-call after upgrades).
func Load() error {
	// Parse layout first so its defines exist; then all pages. Every page
	// redefines the same slot names, so pages parse in SEPARATE sets.
	// We store one set per page: map[pageName]*Template.
	layout, err := template.New("").Funcs(template.FuncMap{
		"Icon": func(name string) template.HTML {
			return template.HTML(iconFunc(name))
		},
		"IconSize": func(name string, size int) template.HTML {
			return template.HTML(iconFunc(name))
		},
	}).ParseFS(templateFS, "templates/_layout.html")
	if err != nil {
		return err
	}
	sets := map[string]*template.Template{}
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "_layout.html" || !strings.HasSuffix(name, ".html") {
			continue
		}
		// Clone layout defines into a fresh set, then overlay the page.
		clone := template.Must(layout.Clone())
		page, err := clone.New(name).Funcs(template.FuncMap{
			"Icon": func(name string) template.HTML {
				return template.HTML(iconFunc(name))
			},
			"IconSize": func(name string, size int) template.HTML {
				return template.HTML(iconFunc(name))
			},
		}).ParseFS(templateFS, "templates/"+name)
		if err != nil {
			return err
		}
		sets[name] = page
	}
	templates.Store(sets)
	return nil
}

// Render writes a page. pageName is the template file ("chat.html");
// standalone pages (login.html) execute directly, extending pages execute
// "base" from the layout with the page's defines.
func Render(w io.Writer, pageName string, data PageData) error {
	sets := templates.Load().(map[string]*template.Template)
	tpl, ok := sets[pageName]
	if !ok {
		return fs.ErrNotExist
	}
	name := "base"
	if pageName == "login.html" {
		name = pageName // standalone
	}
	return tpl.ExecuteTemplate(w, name, data)
}

// StaticHandler returns an http handler serving the embedded assets with
// immutable caching keyed by the static version (atomic so hot reloads can
// bump it).
func StaticHandler(ver *atomic.Value) http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long cache is safe: URLs carry ?v= which changes per build.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	}))
}
