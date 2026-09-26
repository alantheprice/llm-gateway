//go:build ignore

// generate_guide.go — renders ../docs/*.md into internal/server/guide/*.html
// (embedded via go:embed). Run: go run generate_guide.go  (or go generate ./internal/server)
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"llmgateway/internal/server"
)

func main() {
	docs := os.Getenv("DOCS_DIR")
	if docs == "" {
		docs = "../docs"
	}
	out := os.Getenv("OUT_DIR")
	if out == "" {
		out = "guide"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	for _, slug := range []string{"start", "install", "ninfer-engine", "operations"} {
		in := filepath.Join(docs, slug+".md")
		md, err := os.ReadFile(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", in, err)
			continue
		}
		html, err := server.MDToHTML(md)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, slug+".html"), html, 0o644); err != nil {
			panic(err)
		}
		fmt.Println("rendered", in, "->", filepath.Join(out, slug+".html"))
	}
}
