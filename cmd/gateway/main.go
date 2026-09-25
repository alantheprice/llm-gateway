// llm-gateway: single-binary Go port of the Python gateway's inference plane.
// See docs/SPEC.md for the behavioral contract.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/config"
	"llmgateway/internal/server"
)

func main() {
	confPath := os.Getenv("LLM_GATEWAY_CONF")
	if confPath == "" {
		confPath = "llm_gateway.conf"
	}
	cfg, err := config.Load(confPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if p := os.Getenv("PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			cfg.Gateway.Port = port
		}
	}

	usersPath := cfg.Gateway.UsersFile
	if usersPath == "" {
		usersPath = "users.json"
	}
	store, err := auth.Open(usersPath)
	if err != nil {
		log.Fatalf("users store: %v", err)
	}

	srv := server.New(cfg, store)
	srv.ParseNetworks()
	if err := server.InitUI(); err != nil {
		log.Fatalf("templates: %v", err)
	}

	// Initial discovery + first metrics snapshot.
	srv.Discover()
	srv.PollOnce()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background: metrics polling + config hot-reload + usage flush.
	go func() {
		tick := time.NewTicker(time.Duration(cfg.Metrics.PollInterval) * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				srv.PollOnce()
			}
		}
	}()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if nc, changed := cfg.PollWatch(); changed {
					log.Printf("Configuration auto-reloaded (file changed)")
					// Swap what we can safely: pools/thresholds live in cfg
					// read under srv.mu per request; replace the pointer.
					srv.SetConfig(nc)
					cfg = nc
				}
			}
		}
	}()
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				srv.FlushUsage()
			}
		}
	}()

	addr := ":" + strconv.Itoa(cfg.Gateway.Port)
	log.Printf("llm-gateway (go) listening on %s (conf: %s)", addr, confPath)
	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatal(err)
	}
}
