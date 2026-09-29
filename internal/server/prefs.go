package server

import (
	"encoding/json"
	"io"
	"net/http"

	"llmgateway/internal/auth"
)

// handleAPIPrefs: GET the signed-in user's preferences; POST a partial
// update ({"theme": "light"}, {"chat_model": "…"}, {"chat_web": true}).
func (s *Server) handleAPIPrefs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		errBody(w, http.StatusUnauthorized, "login required")
		return
	}
	p := s.store.PrefsOf(sess.U)
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		var in struct {
			Theme     *string `json:"theme"`
			ChatModel *string `json:"chat_model"`
			ChatWeb   *bool   `json:"chat_web"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
			errBody(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if in.Theme != nil {
			switch *in.Theme {
			case "", "dark", "light":
				p.Theme = *in.Theme
			default:
				errBody(w, http.StatusBadRequest, "theme is dark or light")
				return
			}
		}
		if in.ChatModel != nil {
			if len(*in.ChatModel) > 200 {
				errBody(w, http.StatusBadRequest, "model name too long")
				return
			}
			p.ChatModel = *in.ChatModel
		}
		if in.ChatWeb != nil {
			p.ChatWeb = in.ChatWeb
		}
		if err := s.store.SetPrefs(sess.U, p); err != nil {
			errBody(w, http.StatusInternalServerError, "save failed")
			return
		}
	} else if r.Method != http.MethodGet {
		errBody(w, http.StatusMethodNotAllowed, "GET or POST")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(prefsView(p))
}

func prefsView(p auth.UserPrefs) map[string]any {
	return map[string]any{"theme": p.Theme, "chat_model": p.ChatModel, "chat_web": p.ChatWeb != nil && *p.ChatWeb,
		"chat_web_set": p.ChatWeb != nil}
}

// handleSettingsPage: /settings — account, appearance, chat defaults,
// connectors and data in one place.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSessionPage(w, r); !ok {
		return
	}
	s.renderPage(w, r, "settings.html", "settings", "Settings")
}
