package auth

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"
)

// CreateUIKey mints a UI key for a user. Prior ui keys are kept as a small
// ring (newest 3 including the new one): each gateway surface (8033/8034/
// 8035) mints on restart/login, and a hard clobber let one surface's mint
// kill every other surface's live session (observed 2026-09-25: agent-mode
// chat 401'd 13s after an admin login on another port). A bounded ring
// keeps orphan accumulation capped while surviving cross-surface mints.
// key_id = "ui-" + plaintext[3:11] (Python parity).
func (s *Store) CreateUIKey(username, role string) (string, *KeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.LocalKeys[username]
	var named, uis []*KeyRecord
	for _, k := range keys {
		if k.UI {
			uis = append(uis, k)
		} else {
			named = append(named, k)
		}
	}
	// keep the newest 2 prior ui keys (list order = mint order)
	if len(uis) > 2 {
		uis = uis[len(uis)-2:]
	}
	plain, prefix, salt := NewAPIKey()
	rec := &KeyRecord{
		KeyID:   "ui-" + strings.TrimPrefix(plain[:11], "sk-"),
		Prefix:  prefix,
		Salt:    salt,
		Created: time.Now().UTC().Format(isoLayout),
		Active:  true,
		Role:    role,
		UI:      true,
	}
	rec.KeyHash = HashSecret(plain, salt)
	uis = append(uis, rec)
	s.LocalKeys[username] = append(named, uis...)
	if err := s.saveLocked(); err != nil {
		return "", nil, err
	}
	return plain, rec, nil
}

// RoleOf returns the persisted role of a user's most relevant key (ui first,
// then any record with a role), or "" if unknown. Used for admin Bearer
// checks without a PB round-trip (Python: key.role persisted at mint time).
func (s *Store) RoleOf(username string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fallback string
	for _, k := range s.LocalKeys[username] {
		if k.Role != "" {
			if k.UI {
				return k.Role
			}
			fallback = k.Role
		}
	}
	return fallback
}

// SetKeyRoleIfDiffers records the mint-time role on the key matching the
// plaintext prefix (Python chat/config parity). Cheap no-op when equal.
func (s *Store) SetKeyRoleIfDiffers(username, plaintext, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.LocalKeys[username] {
		if k.Prefix != "" && strings.HasPrefix(plaintext, k.Prefix) {
			if k.Role != role {
				k.Role = role
				_ = s.saveLocked()
			}
			return
		}
	}
}

// SetAllKeyRoles updates role on every key of the user (set_role parity).
func (s *Store) SetAllKeyRoles(username, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, k := range s.LocalKeys[username] {
		if k.Role != role {
			k.Role = role
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

// DropUIKeys removes ui keys for a user (stale plaintexts after restarts).
func (s *Store) DropUIKeys(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.LocalKeys[username]
	out := keys[:0]
	for _, k := range keys {
		if !k.UI {
			out = append(out, k)
		}
	}
	s.LocalKeys[username] = out
	_ = s.saveLocked()
}

// DisableUser deactivates every key of the user (disable_user parity).
func (s *Store) DisableUser(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.LocalKeys[username] {
		k.Active = false
		k.Rotating = false
		k.GraceUntil = nil
	}
	_ = s.saveLocked()
}

// PurgeUser removes all local keys (delete_user parity).
func (s *Store) PurgeUser(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.LocalKeys, username)
	_ = s.saveLocked()
}

// MustChangePW / SetMustChangePW manage the first-login fence.
func (s *Store) MustChangePW(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.MustChangePWMap[username]
}

func (s *Store) SetMustChangePW(username string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v {
		s.MustChangePWMap[username] = true
	} else {
		delete(s.MustChangePWMap, username)
	}
	_ = s.saveLocked()
}

// KeyView is the redacted, usage-annotated key listing entry.
type KeyView struct {
	KeyID      string         `json:"key_id"`
	Prefix     string         `json:"prefix"`
	Created    string         `json:"created"`
	Active     bool           `json:"active"`
	Rotating   bool           `json:"rotating"`
	GraceUntil *string        `json:"grace_until"`
	UI         bool           `json:"ui"`
	Usage      map[string]int `json:"usage"`
}

// ListKeys renders redacted key views (Python /keys GET parity).
func (s *Store) ListKeys(username string) []KeyView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []KeyView{}
	for _, k := range s.LocalKeys[username] {
		out = append(out, KeyView{
			KeyID: k.KeyID, Prefix: k.Prefix, Created: k.Created,
			Active: k.Active, Rotating: k.Rotating, GraceUntil: k.GraceUntil, UI: k.UI,
			Usage: map[string]int{"requests": 0, "prompt_tokens": 0, "output_tokens": 0},
		})
	}
	return out
}

// KeyUsageTallies snapshots the per-key usage counters for username from a
// UsageFile-shaped map (kept decoupled to avoid an import cycle).
type UsageReader interface {
	KeyUsage(username string) map[string]map[string]int
}

var _ = json.Marshal
var _ = time.Now
var _ = rand.Read
