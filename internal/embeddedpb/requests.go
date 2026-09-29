package embeddedpb

import (
	"fmt"
	"strings"
	"time"
)

// InitRequestsSchema: create the table + indexes (idempotent).
// Record: one completed inference request (wire type shared with the
// analytics package).
type RequestRecord struct {
	TS        time.Time
	User      string
	KeyID     string
	Model     string
	Backend   string
	Kind      string
	Status    int
	Prompt    int64
	Cached    int64
	Output    int64
	TTFTms    float64
	Prefillms float64
	Decodems  float64
	Totalms   float64
	TokPerSec float64
	ReusePath string
	// Speculative decoding (engine timings); 0 when not reported.
	DraftN        int64
	DraftAccepted int64
	// Engine-reported queue wait before scheduling (stats block); 0 if unknown.
	QueueWaitms float64
	// Requested: the model name the client called (a pool, alias or engine
	// id); Model is what the serving engine was asked for.
	Requested string
}

func (a *App) InitRequestsSchema() error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	_, err := a.pb.DB().NewQuery(`
		CREATE TABLE IF NOT EXISTS requests (
			ts         TEXT NOT NULL,        -- RFC3339 UTC
			ts_hour    TEXT NOT NULL,        -- YYYY-MM-DDTHH (rollup key)
			day        TEXT NOT NULL,        -- YYYY-MM-DD (UTC day)
			user       TEXT NOT NULL,
			key_id     TEXT NOT NULL DEFAULT '',
			model      TEXT NOT NULL DEFAULT '',
			backend    TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT 'chat',
			status     INTEGER NOT NULL DEFAULT 0,
			prompt     INTEGER NOT NULL DEFAULT 0,
			cached     INTEGER NOT NULL DEFAULT 0,
			output     INTEGER NOT NULL DEFAULT 0,
			ttft_ms    REAL NOT NULL DEFAULT 0,
			prefill_ms REAL NOT NULL DEFAULT 0,
			decode_ms  REAL NOT NULL DEFAULT 0,
			total_ms   REAL NOT NULL DEFAULT 0,
			tok_per_s  REAL NOT NULL DEFAULT 0,
			reuse_path TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests (ts);
		CREATE INDEX IF NOT EXISTS idx_requests_day_user ON requests (day, user);
		CREATE INDEX IF NOT EXISTS idx_requests_day_backend ON requests (day, backend);
	`).Execute()
	if err != nil {
		return err
	}
	// Columns added after the first release: add each if missing.
	for _, col := range []string{"draft_n", "draft_accepted", "queue_ms", "requested_model"} {
		var r struct {
			N int64 `db:"n"`
		}
		if err := a.pb.DB().NewQuery(
			`SELECT COUNT(*) AS n FROM pragma_table_info('requests') WHERE name = {:col}`,
		).Bind(map[string]any{"col": col}).One(&r); err != nil {
			return err
		}
		if r.N == 0 {
			if _, err := a.pb.DB().NewQuery(
				"ALTER TABLE requests ADD COLUMN " + col + " " + colType(col) + " NOT NULL DEFAULT " + colDefault(col),
			).Execute(); err != nil {
				return err
			}
		}
	}
	return nil
}

// InsertRequests: batch insert.
func (a *App) InsertRequests(recs []RequestRecord) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	for _, r := range recs {
		_, err := a.pb.DB().NewQuery(`
			INSERT INTO requests (ts, ts_hour, day, user, key_id, model, backend,
				kind, status, prompt, cached, output,
				ttft_ms, prefill_ms, decode_ms, total_ms, tok_per_s, reuse_path,
				draft_n, draft_accepted, queue_ms, requested_model)
			VALUES ({:ts}, {:ts_hour}, {:day}, {:user}, {:key_id}, {:model},
				{:backend}, {:kind}, {:status}, {:prompt}, {:cached}, {:output},
				{:ttft}, {:prefill}, {:decode}, {:total}, {:tps}, {:reuse},
				{:draft_n}, {:draft_acc}, {:queue}, {:requested})
		`).Bind(map[string]any{
			"ts":      r.TS.UTC().Format(time.RFC3339Nano),
			"ts_hour": r.TS.UTC().Format("2006-01-02T15"),
			"day":     r.TS.UTC().Format("2006-01-02"),
			"user":    r.User, "key_id": r.KeyID, "model": r.Model,
			"backend": r.Backend, "kind": r.Kind, "status": r.Status,
			"prompt": r.Prompt, "cached": r.Cached, "output": r.Output,
			"ttft": r.TTFTms, "prefill": r.Prefillms, "decode": r.Decodems,
			"total": r.Totalms, "tps": r.TokPerSec, "reuse": r.ReusePath,
			"draft_n": r.DraftN, "draft_acc": r.DraftAccepted, "queue": r.QueueWaitms,
			"requested": r.Requested,
		}).Execute()
		if err != nil {
			return err
		}
	}
	return nil
}

// PruneRequests: delete raw rows older than the retention window.
func (a *App) PruneRequests(days int) (int64, error) {
	if a.pb.DB() == nil {
		return 0, fmt.Errorf("analytics: DB not open")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	res, err := a.pb.DB().NewQuery(
		`DELETE FROM requests WHERE day < {:cutoff}`,
	).Bind(map[string]any{"cutoff": cutoff}).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SanitizeLabel: strip characters that would confuse group-by output.
func SanitizeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '=' || r == '"' || r == '\'' {
			return -1
		}
		return r
	}, s)
}

// HourlyRow: pre-aggregated hourly analytics row.
type HourlyRow struct {
	Hour      string  `db:"hour" json:"hour"`
	Requests  int64   `db:"requests" json:"requests"`
	Prompt    int64   `db:"prompt" json:"prompt"`
	Output    int64   `db:"output" json:"output"`
	Errors    int64   `db:"errors" json:"errors"`
	TTFTP50   float64 `db:"ttft_p50" json:"ttft_p50"`
	TTFTP95   float64 `db:"ttft_p95" json:"ttft_p95"`
	TokPerSec float64 `db:"tok_per_s" json:"tok_per_s"`
}

// QueryAnalyticsHourly: hourly traffic + latency series (last N days).
// p50/p95 via ordered-set aggregate: substring trick over grouped rows.
func (a *App) QueryAnalyticsHourly(days int, dest *[]HourlyRow) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT ts_hour || ':00' AS hour,
			COUNT(*) AS requests,
			SUM(prompt) AS prompt,
			SUM(output) AS output,
			SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS errors,
			ROUND(AVG(ttft_ms), 0) AS ttft_avg,
			ROUND(MAX(ttft_ms), 0) AS ttft_max,
			ROUND(AVG(tok_per_s), 0) AS tok_per_s
		FROM requests
		WHERE day >= {:start}
		GROUP BY ts_hour ORDER BY ts_hour
	`).Bind(map[string]any{"start": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")}).All(dest)
}

// GPURow: per-backend aggregates.
type GPURow struct {
	Backend     string  `db:"backend" json:"backend"`
	Requests    int64   `db:"requests" json:"requests"`
	Input       int64   `db:"input" json:"input"`
	Output      int64   `db:"output" json:"output"`
	Cached      int64   `db:"cached" json:"cached"`
	CacheHitPct float64 `db:"cache_hit_pct" json:"cache_hit_pct"`
	TokPerSec   float64 `db:"tok_per_s" json:"tok_per_s"`
	TTFTMax     float64 `db:"ttft_max" json:"ttft_max"`
	// Averages over requests that reported the value (zeros = unknown).
	TTFTAvg  float64 `db:"ttft_avg" json:"ttft_avg"`
	TTFTN    int64   `db:"ttft_n" json:"ttft_n"`
	QueueAvg float64 `db:"queue_avg" json:"queue_avg"`
	QueueN   int64   `db:"queue_n" json:"queue_n"`
	// Speculative decoding acceptance across requests that reported it.
	DraftN         int64   `db:"draft_n" json:"draft_n"`
	DraftAccepted  int64   `db:"draft_accepted" json:"draft_accepted"`
	DraftAcceptPct float64 `db:"draft_accept_pct" json:"draft_accept_pct"`
}

// QueryAnalyticsPerGPU: per-backend request/token/latency aggregates.
func (a *App) QueryAnalyticsPerGPU(days int, dest *[]GPURow) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT backend,
			COUNT(*) AS requests,
			SUM(prompt) AS input,
			SUM(output) AS output,
			SUM(cached) AS cached,
			ROUND(100.0 * SUM(cached) / MAX(SUM(prompt), 1), 1) AS cache_hit_pct,
			ROUND(AVG(tok_per_s), 0) AS tok_per_s,
			ROUND(MAX(ttft_ms), 0) AS ttft_max,
			COALESCE(ROUND(AVG(NULLIF(ttft_ms, 0)), 0), 0) AS ttft_avg,
			COUNT(NULLIF(ttft_ms, 0)) AS ttft_n,
			COALESCE(ROUND(AVG(NULLIF(queue_ms, 0)), 0), 0) AS queue_avg,
			COUNT(NULLIF(queue_ms, 0)) AS queue_n,
			SUM(draft_n) AS draft_n,
			SUM(draft_accepted) AS draft_accepted,
			ROUND(100.0 * SUM(draft_accepted) / MAX(SUM(draft_n), 1), 1) AS draft_accept_pct
		FROM requests
		WHERE day >= {:start}
		GROUP BY backend ORDER BY SUM(output) DESC
	`).Bind(map[string]any{"start": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")}).All(dest)
}

// UserRow: per-user aggregates.
type UserRow struct {
	User      string  `db:"user" json:"user"`
	Requests  int64   `db:"requests" json:"requests"`
	Prompt    int64   `db:"prompt" json:"prompt"`
	Output    int64   `db:"output" json:"output"`
	Cached    int64   `db:"cached" json:"cached"`
	CachedPct float64 `db:"cached_pct" json:"cached_pct"`
	TTFTP50   float64 `db:"ttft_p50" json:"ttft_p50"`
	Errors    int64   `db:"errors" json:"errors"`
}

// QueryAnalyticsPerUser: per-user request/token/latency aggregates.
func (a *App) QueryAnalyticsPerUser(days int, dest *[]UserRow) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT user,
			COUNT(*) AS requests,
			SUM(prompt) AS prompt,
			SUM(output) AS output,
			SUM(cached) AS cached,
			ROUND(100.0 * SUM(cached) / MAX(SUM(prompt), 1), 1) AS cached_pct,
			ROUND(AVG(ttft_ms), 0) AS ttft_avg,
			SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS errors
		FROM requests
		WHERE day >= {:start}
		GROUP BY user ORDER BY SUM(output) DESC
	`).Bind(map[string]any{"start": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")}).All(dest)
}

// BackendUserRow: one user's traffic on one backend.
type BackendUserRow struct {
	Backend  string  `db:"backend" json:"backend"`
	User     string  `db:"user" json:"user"`
	Requests int64   `db:"requests" json:"requests"`
	Prompt   int64   `db:"prompt" json:"prompt"`
	Output   int64   `db:"output" json:"output"`
	TTFTAvg  float64 `db:"ttft_avg" json:"ttft_avg"`
	Errors   int64   `db:"errors" json:"errors"`
	LastDay  string  `db:"last_day" json:"last_day"`
}

// QueryBackendUsers: per-(backend, user) aggregates for a set of backends —
// what a GPU owner sees about who used their GPU.
func (a *App) QueryBackendUsers(days int, backends []string, dest *[]BackendUserRow) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	if len(backends) == 0 {
		return nil
	}
	params := map[string]any{"start": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")}
	in := make([]string, len(backends))
	for i, b := range backends {
		k := fmt.Sprintf("b%d", i)
		params[k] = b
		in[i] = "{:" + k + "}"
	}
	return a.pb.DB().NewQuery(`
		SELECT backend, user,
			COUNT(*) AS requests,
			SUM(prompt) AS prompt,
			SUM(output) AS output,
			ROUND(AVG(ttft_ms), 0) AS ttft_avg,
			SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS errors,
			MAX(day) AS last_day
		FROM requests
		WHERE day >= {:start} AND backend IN (` + strings.Join(in, ",") + `)
		GROUP BY backend, user ORDER BY SUM(output) DESC
	`).Bind(params).All(dest)
}

// BackendTokens: routed prompt+output tokens one backend served.
type BackendTokens struct {
	Backend  string `db:"backend" json:"backend"`
	Tokens   int64  `db:"tokens" json:"tokens"`
	Requests int64  `db:"requests" json:"requests"`
}

// QueryBackendTokensDay: tokens each backend served on a UTC day
// (successful requests) — the gateway's own count, unaffected by engine
// restarts resetting their counters.
func (a *App) QueryBackendTokensDay(day string, dest *[]BackendTokens) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT backend, SUM(prompt + output) AS tokens, COUNT(*) AS requests
		FROM requests
		WHERE day = {:day} AND status < 400 AND backend != ''
		GROUP BY backend
	`).Bind(map[string]any{"day": day}).All(dest)
}

// NameCount: requests for one model name clients called.
type NameCount struct {
	Name     string `db:"name" json:"name"`
	Requests int64  `db:"requests" json:"requests"`
}

// QueryRequestedDay: successful requests per model name clients called on a
// UTC day. Rows logged before names were recorded count under the engine's
// model id.
func (a *App) QueryRequestedDay(day string, dest *[]NameCount) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT CASE WHEN requested_model = '' THEN model ELSE requested_model END AS name,
			COUNT(*) AS requests
		FROM requests
		WHERE day = {:day} AND status < 400
		GROUP BY name
	`).Bind(map[string]any{"day": day}).All(dest)
}

// ReuseRow: prefix-reuse path distribution.
type ReuseRow struct {
	Path     string `db:"path" json:"path"`
	Requests int64  `db:"requests" json:"requests"`
}

// QueryAnalyticsReuse: reuse-path distribution (miss = empty path).
func (a *App) QueryAnalyticsReuse(days int, dest *[]ReuseRow) error {
	if a.pb.DB() == nil {
		return fmt.Errorf("analytics: DB not open")
	}
	return a.pb.DB().NewQuery(`
		SELECT CASE WHEN reuse_path = '' THEN 'miss' ELSE reuse_path END AS path,
			COUNT(*) AS requests
		FROM requests
		WHERE day >= {:start}
		GROUP BY path ORDER BY requests DESC
	`).Bind(map[string]any{"start": time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")}).All(dest)
}

// colType: SQL type for columns added by migration.
func colType(col string) string {
	switch col {
	case "queue_ms":
		return "REAL"
	case "requested_model":
		return "TEXT"
	}
	return "INTEGER"
}

// colDefault: the migration default for a column added later.
func colDefault(col string) string {
	if colType(col) == "TEXT" {
		return "''"
	}
	return "0"
}

// QueryTTFTPercentile: the q-quantile (0..1) of reported time-to-first-token
// over the window for a set of backends (one GPU may appear under several
// URLs). 0 when there is no data. SQLite has no percentile function, so this
// counts, then seeks to the rank.
func (a *App) QueryTTFTPercentile(days int, backends []string, q float64) (float64, error) {
	if a.pb.DB() == nil || len(backends) == 0 {
		return 0, fmt.Errorf("analytics: DB not open")
	}
	start := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	params := map[string]any{"start": start}
	in := make([]string, len(backends))
	for i, b := range backends {
		k := fmt.Sprintf("b%d", i)
		params[k] = b
		in[i] = "{:" + k + "}"
	}
	where := "day >= {:start} AND ttft_ms > 0 AND backend IN (" + strings.Join(in, ",") + ")"
	var c struct {
		N int64 `db:"n"`
	}
	if err := a.pb.DB().NewQuery("SELECT COUNT(*) AS n FROM requests WHERE " + where).Bind(params).One(&c); err != nil {
		return 0, err
	}
	if c.N == 0 {
		return 0, nil
	}
	params["off"] = int64(q * float64(c.N-1))
	var v struct {
		T float64 `db:"t"`
	}
	err := a.pb.DB().NewQuery("SELECT ttft_ms AS t FROM requests WHERE " + where +
		" ORDER BY ttft_ms LIMIT 1 OFFSET {:off}").Bind(params).One(&v)
	return v.T, err
}
