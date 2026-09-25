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
	"llmgateway/internal/embeddedpb"
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

	// Embedded PocketBase: identity plane runs in-process (one binary,
	// one SQLite). The standalone pocketbase.service must be OFF before
	// starting the gateway — two servers can't hold the same SQLite.
	pbDataDir := os.Getenv("PB_DATA_DIR")
	if pbDataDir == "" {
		pbDataDir = embeddedpb.DataDirFromGatewayConf(usersPath, "")
	}
	pbPort := 8090
	if p := os.Getenv("PB_PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			pbPort = v
		}
	}
	pbApp, err := embeddedpb.Start(embeddedpb.Config{
		DataDir: pbDataDir,
		Port:    pbPort,
	})
	if err != nil {
		log.Fatalf("embedded pocketbase: %v", err)
	}
	if err := pbApp.WaitUntilHealthy("127.0.0.1", pbPort, 15*time.Second); err != nil {
		log.Fatalf("embedded pocketbase: %v (is pocketbase.service still running? stop it first)", err)
	}
	// The health endpoint can be answered by a rogue PB on the same port
	// (bind failure above surfaces async). Only proceed when OUR app is
	// actually bootstrapped — otherwise we'd operate on the wrong database.
	if !pbApp.OpsReady() {
		log.Fatalf("embedded pocketbase: DB not open after health-wait — another process owns port %d? (systemctl stop pocketbase)", pbPort)
	}
	if err := pbApp.InitOpsTables(); err != nil {
		log.Fatalf("ops tables: %v", err)
	}
	// Legacy import: usage.json daily map + cost_history.json merged into
	// SQLite on every boot. Upserts are idempotent; the JSON files stay in
	// place as mirrors (the earlier rename-on-import scattered data across
	// .imported-* copies — never again).
	if n, err := pbApp.ImportLegacyJSON(server.UsagePath(cfg), server.CostHistoryPath(server.UsagePath(cfg))); err != nil {
		log.Printf("legacy usage/cost import: %v (continuing)", err)
	} else if n > 0 {
		log.Printf("merged %d usage/cost rows into SQLite", n)
	}

	srv := server.New(cfg, store)
	srv.SetOps(pbApp)
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
				if err := srv.SyncUsageToOps(); err != nil {
					log.Printf("usage→sqlite sync: %v", err)
				}
			}
		}
	}()
	// Ops retention: keep 365 days of usage + cost history.
	go func() {
		tick := time.NewTicker(6 * time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ops := srv.Ops(); ops != nil {
					if n, err := ops.PruneOlderThan(365); err == nil && n > 0 {
						log.Printf("retention: pruned %d rows older than 365d", n)
					}
				}
			}
		}
	}()

	addr := ":" + strconv.Itoa(cfg.Gateway.Port)
	log.Printf("llm-gateway (go) listening on %s (conf: %s)", addr, confPath)
	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatal(err)
	}
	// Graceful path (systemd stop → ListenAndServe returns): flush usage
	// so restarts lose nothing. The periodic re-import below re-merges any
	// window that ever slips through.
	srv.FlushUsage()
	if ops := srv.Ops(); ops != nil && pbApp.OpsReady() {
		srv.SyncUsageToOps()
	}
}
