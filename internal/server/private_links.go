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
// paused), sorted by name. Empty for anonymous callers.
func (s *Server) privateModels(user string) []privateModel {
	if user == "" {
		return nil
	}
	var out []privateModel
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
			out = append(out, privateModel{
				Name: privateModelName(c.Agent, e.ModelID), URL: link.VirtualURL(c.Agent, e.Port),
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

// platformModelsFor: the platform model names (pools and overflow pairs)
// whose config lists backend — what a link's owner can consent to serve.
func (s *Server) platformModelsFor(backend string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name, pool := range s.cfg.ModelPools {
		for _, m := range pool.Members {
			if m.Backend == backend {
				out = append(out, name)
				break
			}
		}
	}
	for name, pair := range s.cfg.OverflowPairs {
		if pair.FallbackBackend == backend && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
