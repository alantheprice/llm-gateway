package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// Responses name the model the client asked for. Engines echo their own id
// (a pool member's model id, a private link's engine id, an overflow
// target's id); clients see the name they called — "qwen3.8-27b",
// "gpu-b/qwen3.8-27b" — in both JSON bodies and every stream chunk.

type ctxKeyClientModel struct{}

func withClientModel(r *http.Request, model string) *http.Request {
	if model == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyClientModel{}, model))
}

func clientModelOf(r *http.Request) string {
	m, _ := r.Context().Value(ctxKeyClientModel{}).(string)
	return m
}

// setModelField replaces the value of the first unescaped "model" key in a
// JSON text with name. Quotes inside string values are escaped, so the
// pattern only matches real keys; engines emit the top-level model ahead of
// any nested object that could carry one. Unchanged when absent or equal.
func setModelField(p []byte, name string) []byte {
	i := bytes.Index(p, []byte(`"model"`))
	if i < 0 {
		return p
	}
	j := i + len(`"model"`)
	for j < len(p) && (p[j] == ' ' || p[j] == '\t') {
		j++
	}
	if j >= len(p) || p[j] != ':' {
		return p
	}
	j++
	for j < len(p) && (p[j] == ' ' || p[j] == '\t') {
		j++
	}
	if j >= len(p) || p[j] != '"' {
		return p
	}
	end := j + 1
	for end < len(p) && p[end] != '"' {
		if p[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(p) {
		return p
	}
	quoted, _ := json.Marshal(name)
	if bytes.Equal(p[j:end+1], quoted) {
		return p
	}
	out := make([]byte, 0, len(p)+len(quoted))
	out = append(out, p[:j]...)
	out = append(out, quoted...)
	return append(out, p[end+1:]...)
}
