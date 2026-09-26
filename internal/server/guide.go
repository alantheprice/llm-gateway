// Embedded operator guide: docs/*.md rendered to HTML at build time
// (release builds set the docs tag/embed via go:generate; dev builds fall
// back to reading docs/ from disk).
package server

import (
	"bytes"
	"embed"
	"net/http"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed guide/*.html
var guideFS embed.FS

// GuidePages: slug → title, order matters for the index.
var guidePages = []struct{ Slug, Title string }{
	{"start", "Start Here"},
	{"install", "Installation Runbook"},
	{"ninfer-engine", "NInfer Engine Runbook"},
	{"operations", "Operations Runbook"},
}

// guideHandler: GET /guide and /guide/<slug>.
func (s *Server) guideHandler(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.sessionFrom(r)
	slug := strings.TrimPrefix(r.URL.Path, "/guide")
	slug = strings.Trim(slug, "/")
	title := "Guide"
	if slug == "" {
		slug = "start"
	}
	known := false
	for _, p := range guidePages {
		if p.Slug == slug {
			title = p.Title
			known = true
			break
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
	nav := make([]string, 0, len(guidePages))
	for _, p := range guidePages {
		cls := ""
		if p.Slug == slug {
			cls = ` style="font-weight:700"`
		}
		nav = append(nav, `<a href="/guide/`+p.Slug+`"`+cls+`>`+p.Title+`</a>`)
	}
	html2 := `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>` + title + ` — llm-gateway guide</title>` +
		`<style>body{font:16px/1.6 -apple-system,Segoe UI,Roboto,sans-serif;max-width:860px;margin:2rem auto;padding:0 1.2rem;color:#1f2328}nav{display:flex;gap:1rem;flex-wrap:wrap;padding-bottom:1rem;border-bottom:1px solid #d0d7de;margin-bottom:1.5rem;font-size:14px}nav a{color:#0969da;text-decoration:none}h1,h2{line-height:1.25}code,pre{background:#f6f8fa;border-radius:6px}code{padding:.15em .4em}pre{padding:1em;overflow-x:auto}pre code{padding:0;background:none}table{border-collapse:collapse;width:100%;margin:1rem 0}th,td{border:1px solid #d0d7de;padding:.45rem .7rem;text-align:left}th{background:#f6f8fa}</style></head><body>` +
		`<nav>` + strings.Join(nav, "") + `</nav>` +
		string(body) +
		`<p style="margin-top:3rem;font-size:13px;color:#656d76"><a href="/">← back to llm-gateway</a></p></body></html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html2))
	_ = sess
}

// mdToHTML renders markdown bytes to HTML (used by go:generate).
func MDToHTML(md []byte) ([]byte, error) {
	var buf bytes.Buffer
	md2 := goldmark.New(goldmark.WithRendererOptions(html.WithHardWraps()))
	if err := md2.Convert(md, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
