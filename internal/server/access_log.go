package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// Access log: one line per /v1/* request with the status the CLIENT got,
// so an outage shows exactly what callers saw (503s, dropped streams,
// client disconnects) rather than only the gateway's internal fallbacks.

type ctxKeyAccess struct{}

// statusClientClosed: logged when the client disconnected before the
// gateway sent a status line (nginx's 499 convention). Never sent.
const statusClientClosed = 499

// accessInfo is filled in by relay once a backend is chosen.
type accessInfo struct {
	user, model, backend string
	stream               bool
	streamErr            string        // non-EOF error ending a relayed stream
	start                time.Time     // gateway received the request
	ttft                 time.Duration // start → first streamed byte to the client (0 = none)
}

func accessFrom(r *http.Request) *accessInfo {
	ai, _ := r.Context().Value(ctxKeyAccess{}).(*accessInfo)
	return ai
}

// noteAccess records who asked for what as soon as the request is parsed,
// so a request that never reaches a backend (client gave up while it was
// queued or routed, or it was refused) still names its user and model.
// relay() later fills in the backend and the model that actually served it.
func noteAccess(r *http.Request, user, model string, stream bool) {
	if ai := accessFrom(r); ai != nil {
		ai.user, ai.model, ai.stream = user, model, stream
	}
}

// statusRecorder captures status and bytes; it forwards Flush so SSE
// streaming keeps working through the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(p []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(p)
	sr.bytes += int64(n)
	return n, err
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		ai := &accessInfo{start: start}
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r.WithContext(context.WithValue(r.Context(), ctxKeyAccess{}, ai)))
		status := sr.status
		if status == 0 {
			if r.Context().Err() != nil {
				status = statusClientClosed // client left before any response
			} else {
				status = http.StatusOK // handler wrote nothing
			}
		}
		ttft := "-"
		if ai.ttft > 0 {
			ttft = ai.ttft.Round(time.Millisecond).String()
		}
		log.Printf("access %s %s status=%d dur=%s ttft=%s bytes=%d user=%s model=%s backend=%s stream=%t client_gone=%t stream_err=%q",
			r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond), ttft, sr.bytes,
			orDash(ai.user), orDash(ai.model), orDash(ai.backend), ai.stream, r.Context().Err() != nil, ai.streamErr)
	})
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
