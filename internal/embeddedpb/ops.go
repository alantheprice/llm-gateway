// Ops tables in the embedded PocketBase's SQLite: usage_daily and
// cost_history. Replaces usage.json's per-day map and cost_history.json —
// same data, real queries, bounded by retention.
//
// Kept deliberately boring: plain tables via PB's non-concurrent DB
// handle, created on bootstrap if missing, migrated once from the JSON
// files when they exist.
package embeddedpb

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const opsSchema = `
CREATE TABLE IF NOT EXISTS usage_daily (
  day            TEXT NOT NULL,
  user           TEXT NOT NULL,
  kind           TEXT NOT NULL DEFAULT 'chat',
  requests       INTEGER NOT NULL DEFAULT 0,
  prompt_tokens  INTEGER NOT NULL DEFAULT 0,
  cached_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, user, kind)
);
CREATE INDEX IF NOT EXISTS idx_usage_daily_user ON usage_daily (user, day);

CREATE TABLE IF NOT EXISTS cost_history (
  day          TEXT PRIMARY KEY,
  energy_usd   REAL NOT NULL DEFAULT 0,
  overhead_usd REAL NOT NULL DEFAULT 0,
  capital_usd  REAL NOT NULL DEFAULT 0,
  tokens       INTEGER NOT NULL DEFAULT 0,
  value_usd    REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cost_history_day ON cost_history (day);

CREATE TABLE IF NOT EXISTS gpu_daily (
  day          TEXT NOT NULL,
  backend      TEXT NOT NULL,
  tokens       INTEGER NOT NULL DEFAULT 0,   -- gateway-routed (MAX)
  kwh          REAL NOT NULL DEFAULT 0,
  cache_hits   INTEGER NOT NULL DEFAULT 0,   -- engine LIFETIME counter
  engine_input INTEGER NOT NULL DEFAULT 0,   -- engine LIFETIME counter
  day_start_cache INTEGER NOT NULL DEFAULT 0, -- lifetime value at first sight of the day
  day_start_input INTEGER NOT NULL DEFAULT 0, -- lifetime value at first sight of the day
  PRIMARY KEY (day, backend)
);
`

// InitOpsTables creates the schema (idempotent) on the app's data DB.
// Waits for bootstrap (DB handles open) up to 30s; errors otherwise.
func (a *App) InitOpsTables() error {
	deadline := time.Now().Add(30 * time.Second)
	for a.pb.DB() == nil {
		if time.Now().After(deadline) {
			return fmt.Errorf("ops tables: PocketBase DB never opened (bootstrap failed?)")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := a.pb.DB().NewQuery(opsSchema).Execute(); err != nil {
		return err
	}
	// Schema evolution: gpu_daily shipped without cache_hits.
	return a.migrateGPUDaily()
}

// UpsertUsageDaily merges one (day,user,kind) tally into the table.
// Merge is MAX()-based: counters are monotone within a day, so max is
// idempotent — boot re-imports and live upserts converge no matter the
// order or duplication.
func (a *App) UpsertUsageDaily(day, user, kind string, requests, prompt, cached, output int) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO usage_daily (day, user, kind, requests, prompt_tokens, cached_tokens, output_tokens)
		VALUES ({:day}, {:user}, {:kind}, {:requests}, {:prompt}, {:cached}, {:output})
		ON CONFLICT(day, user, kind) DO UPDATE SET
		  requests = MAX(requests, {:requests}),
		  prompt_tokens = MAX(prompt_tokens, {:prompt}),
		  cached_tokens = MAX(cached_tokens, {:cached}),
		  output_tokens = MAX(output_tokens, {:output})
	`).Bind(map[string]any{
		"day": day, "user": user, "kind": kind,
		"requests": requests, "prompt": prompt, "cached": cached, "output": output,
	}).Execute()
	return err
}

// UpsertCostDay replaces a cost_history row (today's row is recomputed
// live; past rows are written once by the JSON import).
// No-op when the DB isn't open.
func (a *App) UpsertCostDay(day string, energy, overhead, capital, value float64, tokens int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	// MAX()-merge: within a completed UTC day the numbers only grow toward
	// their final value, so boot re-imports + live writes converge
	// idempotently (today's row is recomputed live; past days frozen).
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO cost_history (day, energy_usd, overhead_usd, capital_usd, tokens, value_usd)
		VALUES ({:day}, {:energy}, {:overhead}, {:capital}, {:tokens}, {:value})
		ON CONFLICT(day) DO UPDATE SET
		  energy_usd = MAX(energy_usd, {:energy}), overhead_usd = MAX(overhead_usd, {:overhead}),
		  capital_usd = MAX(capital_usd, {:capital}), tokens = MAX(tokens, {:tokens}),
		  value_usd = MAX(value_usd, {:value})
	`).Bind(map[string]any{
		"day": day, "energy": energy, "overhead": overhead,
		"capital": capital, "tokens": tokens, "value": value,
	}).Execute()
	return err
}

// CostHistorySeries: ordered rows for the value-vs-cost chart.
type CostRow struct {
	Day         string  `json:"day"`
	EnergyUSD   float64 `json:"energy_usd"`
	OverheadUSD float64 `json:"overhead_usd"`
	CapitalUSD  float64 `json:"capital_usd"`
	Tokens      int64   `json:"tokens"`
	ValueUSD    float64 `json:"value_usd"`
}

func (r CostRow) Total() float64 { return r.EnergyUSD + r.OverheadUSD + r.CapitalUSD }

// CostSeries returns the last `days` rows, oldest first.
func (a *App) CostSeries(days int) ([]CostRow, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("ops: DB not open")
	}
	if days <= 0 {
		days = 90
	}
	var rows []CostRow
	// Latest N days, returned oldest-first for charting.
	err := a.pb.DB().NewQuery(`
		SELECT day, energy_usd, overhead_usd, capital_usd, tokens, value_usd
		FROM cost_history ORDER BY day DESC LIMIT {:n}
	`).Bind(map[string]any{"n": days}).All(&rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// migrateGPUDaily: the first gpu_daily release shipped without cache_hits;
// pre-existing tables need the column added (idempotent).
func (a *App) migrateGPUDaily() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	// PB's query builder won't One() into a scalar — use a struct.
	// Add each missing column independently (tables from any earlier
	// release may have either, both, or neither).
	type row struct {
		N int64 `db:"n"`
	}
	for _, col := range []string{"cache_hits", "engine_input", "day_start_cache", "day_start_input"} {
		var r row
		if err := a.pb.DB().NewQuery(
			`SELECT COUNT(*) AS n FROM pragma_table_info('gpu_daily') WHERE name = {:col}`,
		).Bind(map[string]any{"col": col}).One(&r); err != nil {
			return err
		}
		if r.N > 0 {
			continue
		}
		ddl := "ALTER TABLE gpu_daily ADD COLUMN " + col + " INTEGER NOT NULL DEFAULT 0"
		if _, err := a.pb.DB().NewQuery(ddl).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// UpsertGPUDaily merges one backend's daily counters. MAX-merge (engines
// are the source of truth; their counters only grow within a day).
func (a *App) UpsertGPUDaily(day, backend string, tokens, cacheHits, engineInput int64, kwh float64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO gpu_daily (day, backend, tokens, kwh, cache_hits, engine_input,
		                       day_start_cache, day_start_input)
		VALUES ({:day}, {:backend}, {:tokens}, {:kwh}, {:cache}, {:engine},
		        {:cache}, {:engine})
		ON CONFLICT(day, backend) DO UPDATE SET
		  tokens = MAX(tokens, {:tokens}),
		  kwh = MAX(kwh, {:kwh}),
		  cache_hits = MAX(cache_hits, {:cache}),
		  engine_input = MAX(engine_input, {:engine})
	`).Bind(map[string]any{"day": day, "backend": backend, "tokens": tokens, "kwh": kwh,
		"cache": cacheHits, "engine": engineInput}).Execute()
	return err
}

// GPUDailyRow: one backend's day.
type GPUDailyRow struct {
	Backend       string  `json:"backend"`
	Tokens        int64   `json:"tokens"` // engine-reported day input (see Costs page note)
	Kwh           float64 `json:"kwh"`
	CacheHits     int64   `json:"cache_hits"`   // engine LIFETIME counter
	EngineInput   int64   `json:"engine_input"` // engine LIFETIME counter
	DayStartCache int64   `json:"day_start_cache"`
	DayStartInput int64   `json:"day_start_input"`
}

// ReplaceCostDay: SET a day's row (backfill repair). Unlike the
// MAX-merge upsert, a smaller corrected value replaces the stored one.
func (a *App) ReplaceCostDay(day string, energy, overhead, capital, value float64, tokens int64) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		INSERT INTO cost_history (day, energy_usd, overhead_usd, capital_usd, tokens, value_usd)
		VALUES ({:day}, {:energy}, {:overhead}, {:capital}, {:tokens}, {:value})
		ON CONFLICT(day) DO UPDATE SET
		  energy_usd = {:energy},
		  overhead_usd = {:overhead},
		  capital_usd = {:capital},
		  tokens = {:tokens},
		  value_usd = {:value}
	`).Bind(map[string]any{"day": day, "energy": energy, "overhead": overhead,
		"capital": capital, "tokens": tokens, "value": value}).Execute()
	return err
}

// QueryUsageDay: raw per-(user,kind) rows for a day — backfill input.
func (a *App) QueryUsageDay(day string, dest any) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("ops: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT user, kind, requests, prompt_tokens, cached_tokens, output_tokens
		FROM usage_daily WHERE day = {:day}
	`).Bind(map[string]any{"day": day}).All(dest)
}

// GPUDaily: today's (or any day's) per-backend counters, oldest first.
func (a *App) GPUDaily(day string) ([]GPUDailyRow, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("ops: DB not open")
	}
	var rows []GPUDailyRow
	err := a.pb.DB().NewQuery(`
		SELECT backend, tokens, kwh, cache_hits, engine_input,
		       day_start_cache, day_start_input
		FROM gpu_daily
		WHERE day = {:day} ORDER BY tokens DESC
	`).Bind(map[string]any{"day": day}).All(&rows)
	return rows, err
}

// PruneOlderThan: retention. Keeps the DB bounded (JSON grew forever).
func (a *App) PruneOlderThan(days int) (int64, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("ops: DB not open")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	res, err := a.pb.DB().NewQuery(`
		DELETE FROM usage_daily WHERE day < {:cutoff}
	`).Bind(map[string]any{"cutoff": cutoff}).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res2, err := a.pb.DB().NewQuery(`
		DELETE FROM cost_history WHERE day < {:cutoff}
	`).Bind(map[string]any{"cutoff": cutoff}).Execute()
	if err != nil {
		return n, err
	}
	n2, _ := res2.RowsAffected()
	res3, err := a.pb.DB().NewQuery(`DELETE FROM gpu_daily WHERE day < {:cutoff}`).Bind(map[string]any{"cutoff": cutoff}).Execute()
	if err != nil {
		return n + n2, err
	}
	n3, _ := res3.RowsAffected()
	return n + n2 + n3, nil
}

// ImportLegacyJSON one-time-migrates usage.json's daily map and
// cost_history.json into SQLite. Upserts merge, so re-importing a growing
// file is SAFE — the file is NOT renamed; it stays as a human-readable
// mirror and the next boot re-merges any new rows.
func (a *App) ImportLegacyJSON(usagePath, costHistoryPath string) (int, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("ops: DB not open")
	}
	imported := 0
	if data, err := os.ReadFile(usagePath); err == nil {
		var uf struct {
			Daily map[string]map[string]struct {
				Requests     int `json:"requests"`
				PromptTokens int `json:"prompt_tokens"`
				OutputTokens int `json:"output_tokens"`
				CachedTokens int `json:"cached_tokens"`
				Kinds        map[string]struct {
					Requests     int `json:"requests"`
					PromptTokens int `json:"prompt_tokens"`
					OutputTokens int `json:"output_tokens"`
					CachedTokens int `json:"cached_tokens"`
				} `json:"kinds"`
			} `json:"daily"`
		}
		if err := json.Unmarshal(data, &uf); err == nil {
			for day, users := range uf.Daily {
				for user, u := range users {
					if len(u.Kinds) > 0 {
						for kind, k := range u.Kinds {
							if k.Requests == 0 && k.PromptTokens == 0 && k.OutputTokens == 0 {
								continue
							}
							if err := a.UpsertUsageDaily(day, user, kind, k.Requests, k.PromptTokens, k.CachedTokens, k.OutputTokens); err == nil {
								imported++
							}
						}
					} else if u.PromptTokens > 0 || u.OutputTokens > 0 {
						if err := a.UpsertUsageDaily(day, user, "chat", u.Requests, u.PromptTokens, u.CachedTokens, u.OutputTokens); err == nil {
							imported++
						}
					}
				}
			}
		}
	}
	return imported, nil
}

// CostHistoryEmpty: whether the ops cost_history table has no rows.
func (a *App) CostHistoryEmpty() (bool, error) {
	if a.pb.DB() == nil {
		return false, fmt.Errorf("ops: DB not open")
	}
	type row struct {
		N int64 `db:"n"`
	}
	var r row
	if err := a.pb.DB().NewQuery(`SELECT COUNT(*) AS n FROM cost_history`).One(&r); err != nil {
		return false, err
	}
	return r.N == 0, nil
}

// ImportCostHistoryJSON: one-time import of the legacy cost_history.json
// mirror (fresh installs only — see main.go gate).
func (a *App) ImportCostHistoryJSON(costHistoryPath string) (int, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("ops: DB not open")
	}
	imported := 0
	data, err := os.ReadFile(costHistoryPath)
	if err != nil {
		return 0, err
	}
	var ch struct {
		Days map[string]struct {
			EnergyUSD   float64 `json:"energy_usd"`
			OverheadUSD float64 `json:"overhead_usd"`
			CapitalUSD  float64 `json:"capital_usd"`
			Tokens      int64   `json:"tokens"`
			ValueUSD    float64 `json:"value_usd"`
		} `json:"days"`
	}
	if err := json.Unmarshal(data, &ch); err != nil {
		return 0, err
	}
	for day, c := range ch.Days {
		if err := a.UpsertCostDay(day, c.EnergyUSD, c.OverheadUSD, c.CapitalUSD, c.ValueUSD, c.Tokens); err == nil {
			imported++
		}
	}
	return imported, nil
}

// renameLegacy: move an imported JSON file aside so it's clear it's been
// consumed (keeps the data, stops re-import).
func RenameLegacy(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		stamp := time.Now().Format("20060102-150405")
		_ = os.Rename(path, path+".imported-"+stamp)
	}
}

// CostRange returns the cost-history rows for UTC days from..to
// (inclusive; "" = unbounded), oldest first.
func (a *App) CostRange(from, to string) ([]CostRow, error) {
	if a.pb.DB() == nil {
		return nil, fmt.Errorf("ops: DB not open")
	}
	if to == "" {
		to = "9999-12-31"
	}
	var rows []CostRow
	err := a.pb.DB().NewQuery(`
		SELECT day, energy_usd, overhead_usd, capital_usd, tokens, value_usd
		FROM cost_history WHERE day >= {:from} AND day <= {:to} ORDER BY day`).
		Bind(map[string]any{"from": from, "to": to}).All(&rows)
	return rows, err
}
