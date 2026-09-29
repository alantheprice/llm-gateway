package server

import (
	"net/http"
	"slices"
	"sort"

	"llmgateway/internal/auth"
	"llmgateway/internal/link"
)

// Private GPU links. A linked engine serves its owner by default: it is
// listed and callable as "<gpu>/<model-id>" for the owner and for users the
// owner names (or every signed-in user, when shared with everyone), and for
// no one else. Anonymous LAN-trusted callers never reach a link. The owner administers their link —
// sharing, pausing, pool consent, usage, the unredacted card and live
// details — the way the platform admin administers the gateway's own GPUs.
//
// Pools are shared model names, so they need both sides: an admin lists the
// link in a pool (config) AND the owner consents to that pool in the link's
// settings. Otherwise any user could put their machine behind a shared name
// and read other people's prompts, or an admin could hand a user's GPU to
// strangers. Links owned by admins are platform infrastructure and serve
// the pools that list them.
//
// Private names resolve after pools, overflow pairs and discovered models,
// so a link can never shadow a platform model.

type privateModel struct {
	Name    string // what callers use: <agent>/<model-id>
	URL     string // virtual backend
	ModelID string // what the engine serves
	Owner   string
	Agent   string
	KeyID   string
	Shared  bool // the caller is not the owner
}

func privateModelName(agent, modelID string) string { return agent + "/" + modelID }

// privateModels: the link engines user may call (owner or shared-with, not
// paused), sorted by name. Empty for anonymous callers. An engine that
// serves a shared model is reached by others through that model (with its
// load balancing), so only its owner gets the direct name.
func (s *Server) privateModels(user string) []privateModel {
	if user == "" {
		return nil
	}
	var out []privateModel
	var pools map[string][]string
	for _, c := range s.linkReg.Conns() {
		if c.Owner == "" {
			continue
		}
		ls := s.store.LinkSettingsOf(c.Owner, c.KeyID)
		if ls.Paused {
			continue
		}
		shared := c.Owner != user
		if shared && !ls.SharedAll && !slices.Contains(ls.SharedWith, user) {
			continue
		}
		for _, e := range c.Engines() {
			u := link.VirtualURL(c.Agent, e.Port)
			if shared {
				if pools == nil {
					pools = s.linkPoolMembership()
				}
				if s.servesAnyPool(u, pools[u]) {
					continue
				}
			}
			out = append(out, privateModel{
				Name: privateModelName(c.Agent, e.ModelID), URL: u,
				ModelID: e.ModelID, Owner: c.Owner, Agent: c.Agent, KeyID: c.KeyID, Shared: shared,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) resolvePrivate(user, model string) (privateModel, bool) {
	for _, pm := range s.privateModels(user) {
		if pm.Name == model {
			return pm, true
		}
	}
	return privateModel{}, false
}

// privateCaller: the identity private links are checked against — a real
// key or a UI session. Anonymous LAN-trusted requests (user "local", no
// key) never reach private links.
func privateCaller(user, keyID string) string {
	if keyID == "" {
		return ""
	}
	return user
}

// requestUser: who is asking, without failing the request — a valid API key
// (not a link token) or a UI session; "" when anonymous.
func (s *Server) requestUser(r *http.Request) string {
	if key := bearerKey(r); key != "" {
		if user, rec, ok := s.store.LookupKey(key); ok && rec.Role != auth.RoleLink {
			return user
		}
	}
	if c, ok := s.sessionFrom(r); ok {
		return c.U
	}
	return ""
}

// linkServesPool: may this pool route to backend? Non-link members always;
// a link needs its owner's consent for this pool unless an admin owns it,
// and never while paused. A link that is not connected is left to the
// normal down handling.
func (s *Server) linkServesPool(backend, pool string) bool {
	if !isLinkURL(backend) {
		return true
	}
	c := s.linkReg.Lookup(backend)
	if c == nil || c.Owner == "" {
		return true
	}
	ls := s.store.LinkSettingsOf(c.Owner, c.KeyID)
	if ls.Paused {
		return false
	}
	return s.store.RoleOf(c.Owner) == "admin" || slices.Contains(ls.Pools, pool)
}

func (s *Server) servesAnyPool(backend string, pools []string) bool {
	for _, p := range pools {
		if s.linkServesPool(backend, p) {
			return true
		}
	}
	return false
}

// ownsLinkURL: user owns the live link serving backend.
func (s *Server) ownsLinkURL(user, backend string) bool {
	c := s.linkReg.Lookup(backend)
	return c != nil && user != "" && c.Owner == user
}

// privateModelNames: the chat UI's extra entries for user.
func (s *Server) privateModelNames(user string) []string {
	var out []string
	for _, pm := range s.privateModels(user) {
		out = append(out, pm.Name)
	}
	return out
}

// linkPoolMembership: link backend URL -> the shared models (pools) whose
// config lists it, sorted. The one place that answers "which pools use
// this linked GPU".
func (s *Server) linkPoolMembership() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for name, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if isLinkURL(m.Backend) && !slices.Contains(out[m.Backend], name) {
				out[m.Backend] = append(out[m.Backend], name)
			}
		}
	}
	for u := range out {
		sort.Strings(out[u])
	}
	return out
}

// platformModelsFor: the shared models whose config lists backend — what a
// link's owner can consent to serve.
func (s *Server) platformModelsFor(backend string) []string {
	return s.linkPoolMembership()[backend]
}

// chatModelGroup: one <optgroup> in the chat model picker.
type chatModelGroup struct {
	Label  string   `json:"label"`
	Models []string `json:"models"`
}

// isChatModel: the name can serve chat, by its capabilities (engine-reported,
// overridden in config, or guessed from the name as a last resort).
func (s *Server) isChatModel(name, user string) bool {
	c, ok := s.capsFor(name, user)
	return ok && c.has(c.Endpoints, "chat")
}

// chatModelGroups: what the chat UI offers — chat models from the public
// catalog (shared pools, standalone models), the user's own and shared
// GPUs, and for admins the individual engines behind pools (for testing
// one GPU directly).
func (s *Server) chatModelGroups(user string, isAdmin bool) []chatModelGroup {
	var groups []chatModelGroup
	var main []string
	for _, e := range s.catalog() {
		if s.isChatModel(e.ID, user) {
			main = append(main, e.ID)
		}
	}
	sort.Strings(main)
	if len(main) > 0 {
		groups = append(groups, chatModelGroup{Label: "Models", Models: main})
	}
	var mine []string
	for _, pm := range s.privateModels(user) {
		if s.isChatModel(pm.Name, user) {
			mine = append(mine, pm.Name)
		}
	}
	if len(mine) > 0 {
		groups = append(groups, chatModelGroup{Label: "Your & shared GPUs", Models: mine})
	}
	if isAdmin {
		seen := map[string]bool{}
		for _, id := range main {
			seen[id] = true
		}
		var members []string
		s.mu.Lock()
		for _, pool := range s.cfg.ModelPools {
			for _, a := range pool.Aliases {
				seen[a] = true // routes to the pool, not one GPU
			}
		}
		for _, pool := range s.cfg.ModelPools {
			for _, m := range pool.Members {
				if m.ModelID != "" && !seen[m.ModelID] {
					seen[m.ModelID] = true
					members = append(members, m.ModelID)
				}
			}
		}
		s.mu.Unlock()
		var reachable []string
		for _, id := range members {
			if u, _ := s.resolve(id); u != "" && s.isChatModel(id, user) {
				reachable = append(reachable, id)
			}
		}
		sort.Strings(reachable)
		if len(reachable) > 0 {
			groups = append(groups, chatModelGroup{Label: "Specific GPU (admin)", Models: reachable})
		}
	}
	return groups
}

// flatModels: the groups' models in order.
func flatModels(groups []chatModelGroup) []string {
	out := []string{}
	for _, g := range groups {
		out = append(out, g.Models...)
	}
	return out
}

// poolFor resolves a model name to its pool: the pool's own name or one of
// its aliases. Returns the pool's canonical name (cache affinity, leader
// and owner consent are keyed by it).
func (s *Server) poolFor(name string) (string, poolCfgT, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.cfg.ModelPools[name]; ok {
		return name, p, true
	}
	for pn, p := range s.cfg.ModelPools {
		if slices.Contains(p.Aliases, name) {
			return pn, p, true
		}
	}
	return "", poolCfgT{}, false
}
