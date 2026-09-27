package server

import (
	"net/http"
)

// handleAnalyticsPage: the Analytics admin page (charts pull from
// /api/analytics).
func (s *Server) handleAnalyticsPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if sess.Role != "admin" {
		errBody(w, 403, "admin only")
		return
	}
	s.renderPage(w, r, "analytics.html", "analytics", "Analytics")
}
