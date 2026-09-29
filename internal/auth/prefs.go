package auth

// UserPrefs: a user's own preferences, kept with their account so they
// follow them between browsers and devices. Empty values mean "default".
type UserPrefs struct {
	Theme     string `json:"theme,omitempty"`      // "dark" | "light"
	ChatModel string `json:"chat_model,omitempty"` // default model in Chat
	ChatWeb   *bool  `json:"chat_web,omitempty"`   // 🌐 Web on when Chat opens
}

// PrefsOf returns user's preferences (zero value if none).
func (s *Store) PrefsOf(user string) UserPrefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Prefs[user]
	if p.ChatWeb != nil {
		v := *p.ChatWeb
		p.ChatWeb = &v
	}
	return p
}

// SetPrefs stores user's preferences.
func (s *Store) SetPrefs(user string, p UserPrefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Prefs == nil {
		s.Prefs = map[string]UserPrefs{}
	}
	if p == (UserPrefs{}) {
		delete(s.Prefs, user)
	} else {
		s.Prefs[user] = p
	}
	return s.saveLocked()
}
