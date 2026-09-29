package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Image prompt improvement: short or vague prompts make weak images. When
// image_prompting.model is set, /v1/images/generations first asks that text
// model to rewrite the prompt into a detailed image description, generates
// from the rewrite, and returns it as data[].revised_prompt (the field
// OpenAI's DALL·E 3 uses for its own rewriting). A request can opt out with
// "enhance_prompt": false. The rewrite is an ordinary chat request made as
// the same caller, so it is routed, limited and accounted like any other.

const defaultImagePromptInstructions = `You write prompts for a text-to-image model.
Rewrite the user's idea as one detailed image prompt: the subject, what it is doing, the setting, composition and framing, lighting, colour palette, and visual style or medium (photo, illustration, 3D render…), with camera and lens details for photographic images.
Keep everything the user asked for, including any exact text that should appear in the image (in quotes). Don't add people, text or objects they didn't imply.
Reply with the prompt only: no preamble, no quotes, no lists, under 120 words.`

type ctxKeyRevisedPrompt struct{}

func revisedPromptOf(r *http.Request) string {
	p, _ := r.Context().Value(ctxKeyRevisedPrompt{}).(string)
	return p
}

// bufferWriter captures an in-process handler's response.
type bufferWriter struct {
	hdr    http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferWriter) Header() http.Header {
	if b.hdr == nil {
		b.hdr = http.Header{}
	}
	return b.hdr
}
func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// imagePromptModel: the configured rewriting model and instructions.
func (s *Server) imagePromptModel() (model, instructions string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	instructions = s.cfg.ImagePrompting.SystemPrompt
	if strings.TrimSpace(instructions) == "" {
		instructions = defaultImagePromptInstructions
	}
	return s.cfg.ImagePrompting.Model, instructions
}

// improveImagePrompt asks the configured text model to rewrite prompt, as
// the caller of r. On any failure it returns the original prompt.
func (s *Server) improveImagePrompt(r *http.Request, prompt string) (string, bool) {
	model, instructions := s.imagePromptModel()
	if model == "" || strings.TrimSpace(prompt) == "" {
		return prompt, false
	}
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": instructions},
			{"role": "user", "content": prompt},
		},
		// Room for thinking models to reason before answering.
		"max_tokens":  2048,
		"temperature": 0.7,
		"stream":      false,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return prompt, false
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Authorization", "Cookie"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.RemoteAddr = r.RemoteAddr
	bw := &bufferWriter{}
	s.handleChat(bw, req)
	if bw.status != http.StatusOK {
		return prompt, false
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(bw.buf.Bytes(), &out) != nil || len(out.Choices) == 0 {
		return prompt, false
	}
	p := cleanImagePrompt(out.Choices[0].Message.Content)
	if p == "" {
		return prompt, false
	}
	return p, true
}

// cleanImagePrompt trims what models add around a prompt.
func cleanImagePrompt(s string) string {
	s = strings.TrimSpace(s)
	// Thinking models may leave a closed <think> block in the content.
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = strings.TrimSpace(s[i+len("</think>"):])
	}
	for _, p := range []string{"Prompt:", "prompt:", "Image prompt:", "**Prompt:**"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	s = strings.Trim(s, "\"'` \n")
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}

// withRevisedPrompt adds revised_prompt to each generated image in an
// images response (unless the engine already set one).
func withRevisedPrompt(body []byte, revised string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	data, ok := m["data"].([]any)
	if !ok {
		return body
	}
	for _, d := range data {
		if item, ok := d.(map[string]any); ok {
			if _, has := item["revised_prompt"]; !has {
				item["revised_prompt"] = revised
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// prepareImageRequest applies prompt improvement to an images request body
// and strips the gateway-only enhance_prompt field before it's forwarded.
func (s *Server) prepareImageRequest(r *http.Request, body []byte) (*http.Request, []byte) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return r, body
	}
	enhance := true
	if v, ok := m["enhance_prompt"].(bool); ok {
		enhance = v
	}
	_, hadFlag := m["enhance_prompt"]
	delete(m, "enhance_prompt")
	changed := hadFlag
	if prompt, _ := m["prompt"].(string); enhance && prompt != "" {
		if revised, ok := s.improveImagePrompt(r, prompt); ok && revised != prompt {
			m["prompt"] = revised
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyRevisedPrompt{}, revised))
			changed = true
		}
	}
	if !changed {
		return r, body
	}
	if nb, err := json.Marshal(m); err == nil {
		body = nb
	}
	return r, body
}
