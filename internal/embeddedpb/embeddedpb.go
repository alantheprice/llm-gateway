// Package embeddedpb runs PocketBase inside the gateway process: one
// binary, one SQLite database for identity AND operational history.
//
// PB MIT-licensed, imported as a framework (github.com/pocketbase/pocketbase).
// The data dir is the existing ~/pb/pb_data (collections + accounts carry
// over untouched — same engine, same schema).
package embeddedpb

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/cmd"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
)

// Config for the embedded PocketBase instance.
type Config struct {
	// DataDir: PB data directory (pb_data with data.db / auxiliary.db).
	DataDir string
	// Bind: 127.0.0.1 only — the gateway proxies everything public.
	Bind string
	Port int
	// Automigrate: apply pb_migrations dir changes (dev convenience; the
	// dir ships with the existing instance).
	Automigrate bool
}

// App wraps the pocketbase app instance.
type App struct {
	pb *pocketbase.PocketBase
}

// Start initializes PB and serves it on a goroutine. PB's RootCmd is
// invoked with pinned args (never os.Args — gateway flags must not leak),
// and its lifecycle (bootstrap + graceful shutdown) runs inside that
// goroutine via PB's own signal handling.
func Start(cfg Config) (*App, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("embeddedpb: DataDir required")
	}
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8090
	}

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  cfg.DataDir,
		HideStartBanner: true,
	})

	// Migration dev-mode: write collection changes to pb_migrations.
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: cfg.Automigrate,
	})

	app.RootCmd.AddCommand(cmd.NewServeCommand(app, false))
	go func() {
		// Execute with pinned args (cobra parses the args slice we hand
		// it, not os.Args). Blocks until shutdown; on failure the
		// health-wait in main is what actually fails the startup.
		app.RootCmd.SetArgs([]string{"serve",
			"--dir", cfg.DataDir,
			"--http", fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)})
		if err := app.Execute(); err != nil {
			log.Printf("embedded PocketBase exited: %v", err)
		}
	}()
	log.Printf("embedded PocketBase starting on %s:%d (data: %s)", cfg.Bind, cfg.Port, cfg.DataDir)
	return &App{pb: app}, nil
}

// OpsReady reports whether the app is bootstrapped (DB handles open).
// All ops methods are unsafe before this is true.
func (a *App) OpsReady() bool {
	return a.pb.DB() != nil
}

// App exposes the core.App interface (DB access for ops tables, record
// queries for identity surfaces that want to go in-process later).
func (a *App) Core() core.App { return a.pb.App }

// WaitUntilHealthy polls the PB health endpoint until it answers or the
// deadline passes (bootstrap is async).
func (a *App) WaitUntilHealthy(bind string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/api/health", bind, port)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("embedded PocketBase not healthy after %v", timeout)
}

var httpClient = &http.Client{Timeout: 2 * time.Second}

// DataDirFromGatewayConf: absolute PB data dir resolved from the gateway's
// users.json location. Resolved from the gateway users.json location (repo parent/pb/pb_data).
// MUST stay absolute — a relative dir silently forks a second PB database
// under the working directory (learned the hard way).
func DataDirFromGatewayConf(usersFile, confDir string) string {
	home, _ := os.UserHomeDir()
	if usersFile != "" {
		abs, err := filepath.Abs(usersFile)
		if err == nil {
			return filepath.Join(filepath.Dir(filepath.Dir(abs)), "pb", "pb_data")
		}
	}
	if confDir != "" && filepath.IsAbs(confDir) {
		return filepath.Join(confDir, "pb_data")
	}
	return filepath.Join(home, "pb", "pb_data")
}
