// fakeengine: minimal OpenAI-compatible server for link E2E tests.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
)

func main() {
	port := flag.Int("port", 9100, "listen port")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "model": r.URL.Path,
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from fake engine"},
				"finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				"prompt_tokens_details": map[string]any{"cached_tokens": 0},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	fmt.Println("listening on", *port)
	http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), nil)
}
