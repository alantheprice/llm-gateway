// llm-gateway: single-binary, engine-aware LLM gateway with embedded
// PocketBase identity and SQLite operational history.
//
// Modes:
//
//	(default)  serve the gateway (embedded PB must own its port)
//	version    print version
//	superuser  passthrough to the embedded PocketBase superuser command
//	           (e.g. `llm-gateway superuser upsert admin@example.com pw`)
//	user       manage the first admin without the UI (see userCmd)
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"llmgateway/internal/auth"
	"llmgateway/internal/config"
	"llmgateway/internal/embeddedpb"
	"llmgateway/internal/server"
)

// version is stamped at release builds: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	server.Version = version
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("llm-gateway " + version)
		return
	}

	confPath := os.Getenv("LLM_GATEWAY_CONF")
	if confPath == "" {
		confPath = "llm_gateway.conf"
	}
	cfg, err := config.Load(confPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if p := os.Getenv("PORT"); p != "" {
		if port, err := strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			log.Fatalf("invalid PORT %q", p)
		} else {
			cfg.Gateway.Port = port
		}
	}

	usersPath := cfg.Gateway.UsersFile
	if usersPath == "" {
		usersPath = "users.json"
	}

	// Embedded PocketBase app (identity plane + ops SQLite). Built before
	// the mode switch so `superuser` and `user` modes can use it too.
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
	// The server's PB client defaults to 8090; when PB_PORT moves the
	// embedded instance, point the client at it (unless set explicitly).
	if os.Getenv("POCKETBASE_URL") == "" && os.Getenv("PB_URL") == "" && pbPort != 8090 {
		os.Setenv("POCKETBASE_URL", fmt.Sprintf("http://127.0.0.1:%d", pbPort))
	}

	// CLI management modes (run before serving; safe while gateway down).
	if len(os.Args) > 1 && os.Args[1] == "superuser" {
		embeddedpb.SuperuserCmd(pbDataDir, pbPort, os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "user" {
		if err := userCmd(pbDataDir, pbPort, os.Args[2:]); err != nil {
			log.Fatalf("user: %v", err)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "key" {
		if err := keyCmd(usersPath, os.Args[2:]); err != nil {
			log.Fatalf("key: %v", err)
		}
		return
	}

	store, err := auth.Open(usersPath)
	if err != nil {
		log.Fatalf("users store: %v", err)
	}

	// Shutdown ordering: PocketBase catches SIGTERM itself and would close
	// its databases while requests are still finishing (their analytics
	// rows then failed with "DB not open"). Its terminate hook waits here
	// until the gateway has drained and flushed (bounded, inside systemd's
	// 90 s stop timeout).
	drained := make(chan struct{})
	pbApp, pbErr := embeddedpb.Start(embeddedpb.Config{
		DataDir: pbDataDir,
		Port:    pbPort,
		BeforeTerminate: func() {
			select {
			case <-drained:
			case <-time.After(shutdownDrain + 10*time.Second):
				log.Printf("shutdown: gateway drain took too long; closing databases anyway")
			}
		},
	})
	if pbErr != nil {
		log.Fatalf("embedded pocketbase: %v", pbErr)
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
	if err := pbApp.InitRequestsSchema(); err != nil {
		log.Printf("analytics schema: %v (continuing)", err)
	}
	if err := pbApp.InitChatsSchema(); err != nil {
		log.Printf("chat history schema: %v (continuing)", err)
	}
	if err := pbApp.InitDocsSchema(); err != nil {
		log.Printf("document search schema: %v (continuing)", err)
	}
	if err := pbApp.InitVaultSchema(); err != nil {
		log.Printf("vault schema: %v (continuing)", err)
	}
	// Legacy import: usage.json merges on every boot (MAX-upserts are
	// idempotent; usage counters are monotone). cost_history.json imports
	// ONLY while SQLite's cost_history table is empty — its rows are
	// stale mirrors (frozen by the old page-view recording) and re-merging
	// them after a backfill repair would resurrect corrected values.
	costPath := server.CostHistoryPath(server.UsagePath(cfg))
	if n, err := pbApp.ImportLegacyJSON(server.UsagePath(cfg), costPath); err != nil {
		log.Printf("legacy usage/cost import: %v (continuing)", err)
	} else if n > 0 {
		log.Printf("merged %d usage/cost rows into SQLite", n)
	}
	if empty, err := pbApp.CostHistoryEmpty(); err == nil && empty {
		// Fresh SQLite: import once, then retire the JSON mirror.
		if n, err := pbApp.ImportCostHistoryJSON(costPath); err != nil {
			log.Printf("cost_history import: %v (continuing)", err)
		} else {
			log.Printf("imported %d cost_history rows (first boot)", n)
			embeddedpb.RenameLegacy(costPath)
		}
	}

	go func() {
		for {
			if n, err := pbApp.PruneRequests(server.AnalyticsRetentionDays); err != nil && n == 0 {
				log.Printf("analytics prune: %v", err)
			} else if n > 0 {
				log.Printf("analytics: pruned %d old request rows", n)
			}
			time.Sleep(6 * time.Hour)
		}
	}()

	srv := server.New(cfg, store)
	srv.WatchDiskOf(pbDataDir)
	srv.SetOps(pbApp)
	srv.SetEmbeddedPB(pbApp)
	srv.ParseNetworks()
	if err := server.InitUI(); err != nil {
		log.Fatalf("templates: %v", err)
	}

	// Keep live conversations on their GPUs across restarts.
	srv.LoadAffinity()

	// Initial discovery + first metrics snapshot.
	srv.Discover()
	srv.PollOnce()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background: metrics polling + config hot-reload + usage flush.
	// Rediscover engines every minute: models deployed (or removed) after
	// the gateway started are picked up without a restart.
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				srv.Discover()
			}
		}
	}()
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
	// Alerts: problem checks every 30 s (after a first poll has settled).
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				srv.CheckAlerts()
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
				srv.SaveAffinity()
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
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()
	log.Printf("llm-gateway (go) listening on %s (conf: %s)", addr, confPath)
	select {
	case err := <-serveErr:
		log.Fatal(err)
	case <-sigCtx.Done():
	}

	// Graceful stop: refuse new connections, let in-flight requests (streams
	// included) finish, then flush everything that writes to the databases —
	// before PocketBase closes them (see BeforeTerminate above).
	log.Printf("shutdown: draining in-flight requests (up to %s)", shutdownDrain)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownDrain)
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		log.Printf("shutdown: %v (closing remaining connections)", err)
	}
	cancelDrain()
	cancel() // stop pollers and periodic jobs
	srv.FlushUsage()
	srv.SaveAffinity()
	if ops := srv.Ops(); ops != nil && pbApp.OpsReady() {
		if err := srv.SyncUsageToOps(); err != nil {
			log.Printf("shutdown: usage sync: %v", err)
		}
	}
	srv.CloseAnalytics()
	log.Printf("shutdown: flushed; releasing databases")
	close(drained)
	time.Sleep(2 * time.Second) // let PocketBase finish closing
}

// shutdownDrain: how long a stop waits for in-flight requests. systemd's
// stop timeout is 90 s; this plus the flush must fit inside it.
const shutdownDrain = 60 * time.Second
