package server

import (
	"strings"
	"testing"

	"llmgateway/internal/web"
)

// TestEmojiSweepIconRender: the emoji removal swapped the chat composer's
// web/connectors glyphs and the hamburger buttons for Lucide icons (globe,
// plug, menu). A missing icon name renders silently empty, so assert each
// resolves and that a full chat-page render carries real SVGs.
func TestEmojiSweepIconRender(t *testing.T) {
	if err := web.Load(); err != nil {
		t.Fatalf("load templates: %v", err)
	}
	for _, name := range []string{"globe", "plug", "menu", "brain"} {
		if iconSVG(name) == "" {
			t.Errorf("icon %q resolved to empty", name)
		}
	}
	var buf strings.Builder
	if err := web.Render(&buf, "chat.html", web.PageData{Nav: "chat", Role: "user", StaticVer: "1"}); err != nil {
		t.Fatalf("render chat.html: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`<circle cx="12" cy="12" r="10"/>`,     // globe
		`<path d="M12 22v-5"/>`,                // plug
		`<line x1="4" x2="20" y1="6" y2="6"/>`, // menu
		`<strong>Document search</strong>`,
		`<strong>Memory</strong>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("chat page missing %q", want)
		}
	}
}
