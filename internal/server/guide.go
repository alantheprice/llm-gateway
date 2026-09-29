// Embedded operator guide: docs/*.md rendered to HTML at build time
// (release builds set the docs tag/embed via go:generate; dev builds fall
// back to reading docs/ from disk).
package server

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"strings"

	"llmgateway/internal/web"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed guide/*.html
var guideFS embed.FS

// GuidePages: slug → title, order matters for the index. Admin pages are
// operator runbooks: signed-in non-admins don't see them in the tabs (they
// stay reachable by link; nothing in them is secret).
var guidePages = []struct {
	Slug, Title string
	Admin       bool
}{
	{"start", "Start here", false},
	{"link-gpu", "Linking a GPU", false},
	{"mcp", "Connectors (MCP)", false},
	{"install", "Installation", true},
	{"ninfer-engine", "NInfer engine", true},
	{"operations", "Operations", true},
}

// guideHandler: GET /guide and /guide/<slug> — inside the app (menu,
// theme) when signed in, standalone otherwise.
func (s *Server) guideHandler(w http.ResponseWriter, r *http.Request) {
	sess, signedIn := s.sessionFrom(r)
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/guide"), "/")
	if slug == "" {
		slug = "start"
	}
	title, known := "", false
	for _, p := range guidePages {
		if p.Slug == slug {
			title, known = p.Title, true
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	body, err := guideFS.ReadFile("guide/" + slug + ".html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	showAdmin := !signedIn || sess.Role == "admin"
	var tabs strings.Builder
	adminHeader := false
	for _, p := range guidePages {
		if p.Admin && !showAdmin && p.Slug != slug {
			continue
		}
		if p.Admin && !adminHeader {
			tabs.WriteString(`<span class="grp">For operators</span>`)
			adminHeader = true
		}
		cls := ""
		if p.Slug == slug {
			cls = ` class="on" aria-current="page"`
		}
		tabs.WriteString(`<a href="/guide/` + p.Slug + `"` + cls + `>` + template.HTMLEscapeString(p.Title) + `</a>`)
	}
	data := web.PageData{Nav: "help", Title: title, StaticVer: staticVer, Chrome: signedIn,
		Username: sess.U, Role: sess.Role, Extra: map[string]any{
			"tabs": template.HTML(tabs.String()), "body": template.HTML(body)}}
	if sess.U != "" {
		data.Avatar = strings.ToUpper(sess.U[:1])
	}
	if data.Role == "" {
		data.Role = "user"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := web.Render(w, "guide.html", data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// mdToHTML renders markdown bytes to HTML (used by go:generate).
func MDToHTML(md []byte) ([]byte, error) {
	var buf bytes.Buffer
	md2 := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()), // #anchors for deep links from the UI
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	if err := md2.Convert(md, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
