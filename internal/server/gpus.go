package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/link"
	"llmgateway/internal/web"
)

// "My GPUs": users link their own GPUs (outbound link agents) and manage
// them here — separate from API keys. A link token (role=link key) can
// only open an agent connection; it is not an API key and carries no
// account role. Revoking it disconnects the agent at once.
//
// A link is private to its owner until they share it with named users (see
// private_links.go); the owner administers it here: sharing, pausing,
// consent to platform models (pools / overflow) that list it, and who used
// it.

// maxLinksPerUser: link tokens a non-admin may hold at once.
const maxLinksPerUser = 3

func (s *Server) handleGPUsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "gpus.html", "gpus", "My GPUs")
}

// handleModelsPage: /models — every model the user can call and its model
// card. The page reads the same /v1/models endpoints API clients use (with
// the user's UI key), so redaction matches the API exactly.
func (s *Server) handleModelsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "models.html", "models", "Models")
}

// handleImagesPage: /images — text-to-image with the user's own key.
func (s *Server) handleImagesPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "images.html", "images", "Images")
}

// agentEngineView: one engine behind a live agent, as shown on the page.
type agentEngineView struct {
	URL         string         `json:"url"`
	ModelID     string         `json:"model_id"`
	GPULabel    string         `json:"gpu_label"`
	Up          bool           `json:"up"`
	Polled      bool           `json:"polled"`
	Running     int            `json:"running"`
	Lanes       int            `json:"lanes"`
	Pools       []string       `json:"pools"`
	CardSummary map[string]any `json:"card_summary,omitempty"`
	// Model: the private name the owner (and users it is shared with) call.
	Model string `json:"model"`
	// Serving: platform models listing this engine that it actually
	// serves — Pools minus those its owner has not consented to.
	Serving []string `json:"serving"`
}

type agentView struct {
	Name    string            `json:"name"`
	Owner   string            `json:"owner"`
	KeyID   string            `json:"key_id,omitempty"`
	Version string            `json:"agent_version"`
	Since   time.Time         `json:"connected_since"`
	Engines []agentEngineView `json:"engines"`
}

func (s *Server) agentViews(filter func(*link.Conn) bool) []agentView {
	out := []agentView{}
	for _, c := range s.linkReg.Conns() {
		if !filter(c) {
			continue
		}
		av := agentView{Name: c.Agent, Owner: c.Owner, KeyID: c.KeyID, Version: c.Version, Since: c.Since.UTC()}
		for _, e := range c.Engines() {
			u := link.VirtualURL(c.Agent, e.Port)
			ev := agentEngineView{URL: u, ModelID: e.ModelID, GPULabel: s.gpuIdentity(u).Label,
				Up: !s.tracker.IsDown(u), Pools: s.platformModelsFor(u), CardSummary: cardSummary(s.engineInfo(u).card),
				Model: privateModelName(c.Agent, e.ModelID), Serving: []string{}}
			for _, p := range ev.Pools {
				if s.linkServesPool(u, p) {
					ev.Serving = append(ev.Serving, p)
				}
			}
			if l := s.tracker.Get(u); l != nil {
				ev.Polled, ev.Running, ev.Lanes = true, l.Running, l.Lanes
			}
			av.Engines = append(av.Engines, ev)
		}
		out = append(out, av)
	}
	return out
}

// handleAPIGPUs: GET lists the user's links (+ every live agent for
// admins); POST {action: create_link | revoke_link}.
func (s *Server) handleAPIGPUs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	user, isAdmin := sess.U, sess.Role == "admin"
	switch r.Method {
	case http.MethodGet:
		type linkRow struct {
			auth.KeyView
			Agent    *agentView        `json:"agent"`
			Settings auth.LinkSettings `json:"settings"`
			Usage    any               `json:"usage"`
		}
		mine := s.agentViews(func(c *link.Conn) bool { return c.Owner == user })
		rows := []linkRow{}
		for _, k := range s.store.ListLinkKeys(user) {
			row := linkRow{KeyView: k, Settings: s.store.LinkSettingsOf(user, k.KeyID)}
			for i := range mine {
				if mine[i].KeyID == k.KeyID {
					row.Agent = &mine[i]
				}
			}
			row.Usage = s.linkUsage(row.Agent)
			rows = append(rows, row)
		}
		out := map[string]any{
			"links":          rows,
			"server_url":     s.publicBaseURL(),
			"max_links":      maxLinksPerUser,
			"is_admin":       isAdmin,
			"shared_with_me": s.sharedWithMe(user),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	case http.MethodPost:
		var body struct {
			Action  string `json:"action"`
			Name    string `json:"name"`
			Engine  string `json:"engine"`
			ModelID string `json:"model_id"`
			KeyID   string `json:"key_id"`
			// update_link: nil = leave unchanged
			SharedWith *[]string `json:"shared_with"`
			Pools      *[]string `json:"pools"`
			Paused     *bool     `json:"paused"`
			SharedAll  *bool     `json:"shared_all"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			errBody(w, http.StatusBadRequest, "bad json")
			return
		}
		switch body.Action {
		case "create_link":
			s.createLink(w, user, isAdmin, body.Name, body.Engine, body.ModelID)
		case "revoke_link":
			s.revokeLink(w, user, body.KeyID)
		case "update_link":
			s.updateLink(w, user, body.KeyID, body.SharedWith, body.Pools, body.Paused, body.SharedAll)
		default:
			errBody(w, http.StatusBadRequest, "unknown action")
		}
	default:
		errBody(w, http.StatusMethodNotAllowed, "GET/POST only")
	}
}

func (s *Server) createLink(w http.ResponseWriter, user string, isAdmin bool, name, engine, modelID string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !link.ValidAgentLabel(name) {
		errBody(w, http.StatusBadRequest, "GPU name must be 1-63 chars of a-z 0-9 . _ -")
		return
	}
	engine, modelID = strings.TrimSpace(engine), strings.TrimSpace(modelID)
	if engine == "" {
		engine = "127.0.0.1:8000"
	}
	if !validEngineAddr(engine) || modelID == "" || strings.ContainsAny(modelID, " \t\n'\"$`\\") {
		errBody(w, http.StatusBadRequest, "engine must be host:port and model id a plain model name")
		return
	}
	if !isAdmin && s.store.CountActiveLinkKeys(user) >= maxLinksPerUser {
		errBody(w, http.StatusBadRequest, "link limit reached — revoke one first")
		return
	}
	keyID := "link-" + name
	for _, k := range s.store.ListLinkKeys(user) {
		if k.KeyID == keyID && k.Active {
			errBody(w, http.StatusConflict, "you already have a link named "+name)
			return
		}
	}
	plain, rec, err := s.store.CreateKey(user, keyID, auth.RoleLink, false)
	if err != nil {
		errBody(w, http.StatusInternalServerError, err.Error())
		return
	}
	tpl, err := web.StaticFile("link-agent-install.sh.tpl")
	if err != nil {
		errBody(w, http.StatusInternalServerError, "installer template missing")
		return
	}
	script := strings.NewReplacer(
		"__SERVER__", s.publicBaseURL(),
		"__NAME__", name,
		"__ENGINE__", engine+"="+modelID,
		"__TOKEN__", plain,
	).Replace(string(tpl))
	log.Printf("User %s created GPU link %q (key %s)", user, name, rec.KeyID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "ok", "key_id": rec.KeyID, "name": name,
		"virtual_url": link.VirtualURL(name, enginePort(engine)),
		"model":       privateModelName(name, modelID),
		"script":      script,
		"note":        "the script contains the link token and is shown once",
	})
}

func (s *Server) revokeLink(w http.ResponseWriter, user, keyID string) {
	found := false
	for _, k := range s.store.ListLinkKeys(user) {
		if k.KeyID == keyID {
			found = true
		}
	}
	if !found || !s.store.RevokeKey(user, keyID) {
		errBody(w, http.StatusNotFound, "no such link")
		return
	}
	s.store.SetLinkSettings(user, keyID, auth.LinkSettings{})
	n := s.linkReg.DisconnectKey(user, keyID)
	log.Printf("User %s revoked GPU link %s (%d agent(s) disconnected)", user, keyID, n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "disconnected": n})
}

// validEngineAddr: host:port with a numeric port and no shell metacharacters
// (the value is written into the installer script).
func validEngineAddr(s string) bool {
	host, port, ok := strings.Cut(s, ":")
	if !ok || host == "" || port == "" {
		return false
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(port) <= 5
}

func enginePort(addr string) int {
	_, port, _ := strings.Cut(addr, ":")
	n := 0
	for _, r := range port {
		n = n*10 + int(r-'0')
	}
	return n
}

// Version: the gateway's release (set by main from -ldflags); "dev" for
// source builds. Tagged releases fall back to their own GitHub release for
// agent downloads they don't have locally.
var Version = "dev"

// releaseRepo: where releases live (owner/name); overridable for forks.
func releaseRepo() string {
	if r := os.Getenv("LLM_GATEWAY_RELEASE_REPO"); r != "" {
		return r
	}
	return "alantheprice/llm-gateway"
}

// agentPlatforms: platforms a link agent is built for (see deploy build).
var agentPlatforms = map[string]bool{
	"linux-amd64": true, "linux-arm64": true, "darwin-arm64": true, "darwin-amd64": true,
}

// handleAgentDownload: GET /downloads/llm-link-agent-<os>-<arch> — the agent
// binary the installer fetches. Served from bin/llm-link-agent-<os>-<arch>
// next to the gateway binary; linux-amd64 also accepts LLM_LINK_AGENT_BIN
// or bin/llm-link-agent (the native build).
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	platform := strings.TrimPrefix(r.URL.Path, "/downloads/llm-link-agent-")
	if !agentPlatforms[platform] {
		errBody(w, http.StatusNotFound, "no agent build for that platform (have: linux-amd64, linux-arm64, darwin-arm64, darwin-amd64)")
		return
	}
	dir := ""
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	candidates := []string{filepath.Join(dir, "llm-link-agent-"+platform)}
	if platform == "linux-amd64" {
		if p := os.Getenv("LLM_LINK_AGENT_BIN"); p != "" {
			candidates = append([]string{p}, candidates...)
		}
		candidates = append(candidates, filepath.Join(dir, "llm-link-agent"))
	}
	for _, path := range candidates {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			f.Close()
			continue
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="llm-link-agent"`)
		http.ServeContent(w, r, "llm-link-agent", st.ModTime(), f)
		return
	}
	// Not installed next to the gateway: a tagged release fetches the agent
	// from its own GitHub release (same version, so the protocol matches).
	if strings.HasPrefix(Version, "v") {
		http.Redirect(w, r, "https://github.com/"+releaseRepo()+"/releases/download/"+Version+"/llm-link-agent-"+platform, http.StatusFound)
		return
	}
	errBody(w, http.StatusNotFound, "agent binary for "+platform+" not available on this gateway")
}

// maxShares: users one link may be shared with.
const maxShares = 50

// updateLink: the owner's controls — share with named users, consent to
// platform models, pause. Takes effect on the next request.
func (s *Server) updateLink(w http.ResponseWriter, user, keyID string, shared, pools *[]string, paused, sharedAll *bool) {
	active := false
	for _, k := range s.store.ListLinkKeys(user) {
		if k.KeyID == keyID && k.Active {
			active = true
		}
	}
	if !active {
		errBody(w, http.StatusNotFound, "no such link")
		return
	}
	ls := s.store.LinkSettingsOf(user, keyID)
	if shared != nil {
		clean := []string{}
		for _, u := range *shared {
			u = strings.TrimSpace(u)
			if u == "" || u == user || slices.Contains(clean, u) {
				continue
			}
			if !s.knownUser(u) {
				errBody(w, http.StatusBadRequest, "no such user: "+u)
				return
			}
			clean = append(clean, u)
		}
		if len(clean) > maxShares {
			errBody(w, http.StatusBadRequest, fmt.Sprintf("share with at most %d users", maxShares))
			return
		}
		sort.Strings(clean)
		ls.SharedWith = clean
	}
	if pools != nil {
		clean := []string{}
		s.mu.Lock()
		for _, p := range *pools {
			if _, isPool := s.cfg.ModelPools[p]; isPool && !slices.Contains(clean, p) {
				clean = append(clean, p)
			}
		}
		s.mu.Unlock()
		sort.Strings(clean)
		ls.Pools = clean
	}
	if paused != nil {
		ls.Paused = *paused
	}
	if sharedAll != nil {
		ls.SharedAll = *sharedAll
	}
	if err := s.store.SetLinkSettings(user, keyID, ls); err != nil {
		errBody(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("User %s updated GPU link %s: shared=%v everyone=%v pools=%v paused=%v", user, keyID, ls.SharedWith, ls.SharedAll, ls.Pools, ls.Paused)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "settings": ls})
}

// knownUser: an account exists — holds keys here, or is in PocketBase.
func (s *Server) knownUser(u string) bool {
	if s.store.HasUser(u) {
		return true
	}
	if s.pb == nil {
		return false
	}
	rec, err := s.pb.FindUser(u)
	return err == nil && rec != nil
}

// linkUsage: 7-day traffic by user on a link's engines — the owner sees
// who used their GPU. Nil when the agent is offline or analytics is off.
func (s *Server) linkUsage(a *agentView) any {
	ops := s.Ops()
	if a == nil || ops == nil {
		return nil
	}
	var urls []string
	for _, e := range a.Engines {
		urls = append(urls, e.URL)
	}
	var rows []embeddedpb.BackendUserRow
	if err := ops.QueryBackendUsers(7, urls, &rows); err != nil {
		return nil
	}
	return rows
}

// sharedWithMe: live private links other users shared with user.
func (s *Server) sharedWithMe(user string) []map[string]string {
	out := []map[string]string{}
	for _, pm := range s.privateModels(user) {
		if pm.Shared {
			out = append(out, map[string]string{"model": pm.Name, "owner": pm.Owner, "gpu": pm.Agent})
		}
	}
	return out
}
