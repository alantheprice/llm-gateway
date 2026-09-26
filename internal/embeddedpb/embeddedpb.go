// Package embeddedpb runs PocketBase inside the gateway process: one
// binary, one SQLite database for identity AND operational history.
//
// PB MIT-licensed, imported as a framework (github.com/pocketbase/pocketbase).
// The data dir is the existing ~/pb/pb_data (collections + accounts carry
// over untouched — same engine, same schema).
package embeddedpb

import (
	cryptorand "crypto/rand"
	"encoding/json"
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

	// Serve command must be registered BEFORE Execute in the goroutine
	// (Execute does not register system commands).
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

// SuperuserCmd: passthrough to the embedded PocketBase superuser command
// (upsert/list). Runs synchronously against this install's data dir —
// used by install scripts to seed dashboard credentials.
func SuperuserCmd(dataDir string, port int, args []string) {
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dataDir,
		HideStartBanner: true,
	})
	app.RootCmd.AddCommand(cmd.NewSuperuserCommand(app))
	passArgs := append([]string{"superuser"}, args...)
	app.RootCmd.SetArgs(passArgs)
	if err := app.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "superuser:", err)
		os.Exit(1)
	}
}

// BootstrapApp: create + bootstrap (open DBs, run migrations) a PB app
// WITHOUT serving — for CLI management (create-admin, ops backfills).
func BootstrapApp(dataDir string, port int) (*App, error) {
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dataDir,
		HideStartBanner: true,
	})
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{Automigrate: false})
	if err := app.Bootstrap(); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return &App{pb: app}, nil
}

// Close releases the app's DB handles (CLI short-lived processes).
func (a *App) Close() error { return a.pb.ResetBootstrapState() }

// EnsureSuperuserEnv: creates/refreshes the PB superuser account and the
// .superuser-env file (SUPERUSER_PASS=...) inside the PB data dir
// — the location pbSuperuserPass() reads at gateway startup. install.sh
// and create-admin call this so admin-token features work on fresh hosts.
func (a *App) EnsureSuperuserEnv(dataDir string, ident, pass string) (string, error) {
	if pass == "" {
		pass = RandomPassword()
	}
	if ident == "" {
		ident = "admin@llm.local"
	}
	// Create/refresh the dashboard superuser directly (the _superusers
	// auth collection) — same as `pocketbase superuser upsert` but without
	// the cobra dance.
	su, err := BootstrapApp(dataDir, 0)
	if err != nil {
		return "", err
	}
	defer su.Close()
	suCol, err := su.pb.FindCachedCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		return "", fmt.Errorf("superusers collection: %w", err)
	}
	existing, err := su.pb.FindFirstRecordByData(suCol, "email", ident)
	rec := core.NewRecord(suCol)
	if err == nil && existing != nil {
		rec = existing
	}
	rec.Set("email", ident)
	rec.Set("password", pass)
	rec.Set("passwordConfirm", pass)
	if err := su.pb.Save(rec); err != nil {
		return "", fmt.Errorf("save superuser: %w", err)
	}
	// Persist the env file where the gateway's pbSuperuserPass() looks:
	// <pb_data parent>/.superuser-env (~/pb/.superuser-env on this layout).
	envPath := filepath.Join(dataDir, ".superuser-env")
	content := fmt.Sprintf("SUPERUSER_PASS=%s\n", pass)
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		return "", err
	}
	return envPath, nil
}

// RandomPassword: readable 20-char password (no ambiguous glyphs).
func RandomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 20)
	if _, err := cryptorand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// CreateAdminUser upserts a PB user record with role=admin (first-admin
// bootstrap for install scripts). Password is set via PB's record API so
// hashing/validators apply.
func (a *App) CreateAdminUser(username, password string) error {
	col, err := a.ensureRoleField()
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	existing, err := a.pb.FindFirstRecordByData(col, "username", username)
	if err == nil && existing != nil {
		rec = existing
	}
	rec.Set("username", username)
	rec.Set("email", username+"@gateway.local")
	rec.Set("password", password)
	rec.Set("passwordConfirm", password)
	rec.Set("verified", true)
	rec.Set("role", "admin")
	if existing != nil {
		return a.pb.Save(rec)
	}
	return a.pb.SaveNoValidate(rec)
}

// ensureRoleField: fresh PB installs lack the gateway's `role` select
// field on the users collection. Appends it idempotently (GET-modify-PUT —
// PB replaces the whole field list on PATCH).
func (a *App) ensureRoleField() (*core.Collection, error) {
	col, err := a.pb.FindCachedCollectionByNameOrId("users")
	if err != nil {
		return nil, fmt.Errorf("users collection: %w", err)
	}
	hasRole := col.Fields.GetByName("role") != nil
	hasUsername := col.Fields.GetByName("username") != nil
	if hasRole && hasUsername {
		return col, nil // already provisioned
	}

	// 1. username text field (gateway logs in by username — Python parity;
	//    fresh PB 0.40 auth collections don't have one).
	old := *col
	if !hasUsername {
		col.Fields = append(col.Fields, &core.TextField{
			Name: "username", Min: 2, Max: 40, Pattern: `^[\w.\-@]+$`,
		})
	}
	// 2. role select field.
	if !hasRole {
		col.Fields = append(col.Fields, &core.SelectField{
			Name:      "role",
			MaxSelect: 1,
			Values:    []string{"user", "admin"},
		})
	}
	if err := a.pb.SaveNoValidate(col); err != nil {
		return nil, fmt.Errorf("add username/role fields: %w", err)
	}
	// Field adds are physical ALTER TABLEs on the records table — PB only
	// applies them via the explicit sync (Save alone updates the meta).
	if err := a.pb.SyncRecordTableSchema(col, &old); err != nil {
		return nil, fmt.Errorf("sync users table: %w", err)
	}

	// 3. unique index on username (prerequisite for identityFields).
	//    Declared on the COLLECTION's Indexes list — PB validates identity
	//    fields against collection-declared indexes and materializes them
	//    on Save.
	col.Indexes = append(col.Indexes,
		"CREATE UNIQUE INDEX `idx_users_username` ON `users` (`username`)")

	// 4. passwordAuth.identityFields — the options struct is unexported,
	// so round-trip the whole collection through its JSON form (which
	// repopulates the auth options) and save. This triggers PB's
	// collection-save hooks so the live process's cache updates too.
	full, err := json.Marshal(col)
	if err != nil {
		return nil, err
	}
	var asMap map[string]any
	if err := json.Unmarshal(full, &asMap); err != nil {
		return nil, err
	}
	pa, _ := asMap["passwordAuth"].(map[string]any)
	if pa == nil {
		pa = map[string]any{}
	}
	pa["identityFields"] = []string{"email", "username"}
	asMap["passwordAuth"] = pa
	merged, err := json.Marshal(asMap)
	if err != nil {
		return nil, err
	}
	// Collection implements json.Unmarshaler — repopulates the unexported
	// auth options from the merged blob.
	if err := col.UnmarshalJSON(merged); err != nil {
		return nil, err
	}
	if err := a.pb.SaveNoValidate(col); err != nil {
		return nil, fmt.Errorf("set identityFields: %w", err)
	}
	if err := a.pb.ReloadCachedCollections(); err != nil {
		return nil, err
	}
	return a.pb.FindCachedCollectionByNameOrId("users")
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
