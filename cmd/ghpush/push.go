package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Umami's bot-detection blocks non-browser user-agents and silently returns
// {"beep":"boop"} instead of recording the event. Use a browser UA so our
// historical import events are actually stored.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"

// Record mirrors the ghtraffic NDJSON output format.
type Record struct {
	CollectedAt string     `json:"collected_at"`
	Repo        string     `json:"repo"`
	Date        string     `json:"date"`
	Views       DayCounts  `json:"views"`
	Clones      DayCounts  `json:"clones"`
	Referrers   []Referrer `json:"referrers,omitempty"`
	Paths       []Path     `json:"paths,omitempty"`
}

// DayCounts holds total and unique counts for a traffic metric.
type DayCounts struct {
	Count   int `json:"count"`
	Uniques int `json:"uniques"`
}

// Referrer is a traffic source entry.
type Referrer struct {
	Name    string `json:"referrer"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

// Path is a popular-paths entry.
type Path struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

// umamiEvent is a single event in Umami's send/batch API format.
type umamiEvent struct {
	Type    string       `json:"type"`
	Payload eventPayload `json:"payload"`
}

type eventPayload struct {
	Website   string         `json:"website"`
	Hostname  string         `json:"hostname"`
	Screen    string         `json:"screen,omitempty"`
	Language  string         `json:"language,omitempty"`
	Title     string         `json:"title,omitempty"`
	URL       string         `json:"url"`
	Referrer  string         `json:"referrer,omitempty"`
	Name      string         `json:"name,omitempty"`
	Timestamp int64          `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// pushState is the in-memory representation of what has already been pushed.
// It is loaded from and saved to a SQLite database.
type pushState struct {
	// Traffic maps "repo|date" to cumulative counts already pushed.
	Traffic map[string]trafficCounts
	// Referrers and Paths map "repo|collected-date" to a sent flag.
	Referrers map[string]bool
	Paths     map[string]bool
}

type trafficCounts struct {
	Views  int
	Clones int
}

func newPushState() pushState {
	return pushState{
		Traffic:   make(map[string]trafficCounts),
		Referrers: make(map[string]bool),
		Paths:     make(map[string]bool),
	}
}

// openDB opens (or creates) the SQLite state database at path and initialises
// the schema. Pass ":memory:" for an ephemeral in-process database (useful
// in tests).
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("WAL pragma: %w", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS traffic (
			repo   TEXT NOT NULL,
			date   TEXT NOT NULL,
			views  INTEGER NOT NULL DEFAULT 0,
			clones INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, date)
		);
		CREATE TABLE IF NOT EXISTS snapshots (
			repo           TEXT NOT NULL,
			kind           TEXT NOT NULL,
			collected_date TEXT NOT NULL,
			PRIMARY KEY (repo, kind, collected_date)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return db, nil
}

// loadState reads the full push state from the database into memory.
// A nil db returns an empty state without error.
func loadState(db *sql.DB) (pushState, error) {
	st := newPushState()
	if db == nil {
		return st, nil
	}

	rows, err := db.Query(`SELECT repo, date, views, clones FROM traffic`)
	if err != nil {
		return st, fmt.Errorf("query traffic: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var repo, date string
		var views, clones int
		if err := rows.Scan(&repo, &date, &views, &clones); err != nil {
			return st, err
		}
		st.Traffic[repo+"|"+date] = trafficCounts{Views: views, Clones: clones}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows2, err := db.Query(`SELECT repo, kind, collected_date FROM snapshots`)
	if err != nil {
		return st, fmt.Errorf("query snapshots: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var repo, kind, date string
		if err := rows2.Scan(&repo, &kind, &date); err != nil {
			return st, err
		}
		key := repo + "|" + date
		switch kind {
		case "referrer":
			st.Referrers[key] = true
		case "path":
			st.Paths[key] = true
		}
	}
	return st, rows2.Err()
}

// saveState writes the push state to the database using upsert for traffic
// counts and insert-or-ignore for snapshot flags. A nil db is a no-op.
func saveState(db *sql.DB, st pushState) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for key, tc := range st.Traffic {
		repo, date, ok := splitKey(key)
		if !ok {
			continue
		}
		_, err := tx.Exec(`
			INSERT INTO traffic (repo, date, views, clones) VALUES (?, ?, ?, ?)
			ON CONFLICT (repo, date) DO UPDATE SET
				views  = excluded.views,
				clones = excluded.clones
		`, repo, date, tc.Views, tc.Clones)
		if err != nil {
			return fmt.Errorf("upsert traffic %s: %w", key, err)
		}
	}

	for key := range st.Referrers {
		repo, date, ok := splitKey(key)
		if !ok {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO snapshots (repo, kind, collected_date) VALUES (?, 'referrer', ?)`,
			repo, date,
		); err != nil {
			return fmt.Errorf("insert referrer snapshot %s: %w", key, err)
		}
	}

	for key := range st.Paths {
		repo, date, ok := splitKey(key)
		if !ok {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO snapshots (repo, kind, collected_date) VALUES (?, 'path', ?)`,
			repo, date,
		); err != nil {
			return fmt.Errorf("insert path snapshot %s: %w", key, err)
		}
	}

	return tx.Commit()
}

// jsonPushState mirrors the legacy JSON state file format used before the
// SQLite migration. Field names match the original json tags exactly.
type jsonPushState struct {
	Traffic map[string]struct {
		Views  int `json:"views"`
		Clones int `json:"clones"`
	} `json:"traffic"`
	Referrers map[string]bool `json:"referrers"`
	Paths     map[string]bool `json:"paths"`
}

// importJSONState reads a legacy JSON state file and merges its contents into
// the database. Existing rows are updated via upsert; the import is idempotent.
// A nil db is a no-op.
func importJSONState(path string, db *sql.DB) error {
	if db == nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	var js jsonPushState
	if err := json.NewDecoder(f).Decode(&js); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	st := newPushState()
	for key, tc := range js.Traffic {
		st.Traffic[key] = trafficCounts{Views: tc.Views, Clones: tc.Clones}
	}
	for key, v := range js.Referrers {
		st.Referrers[key] = v
	}
	for key, v := range js.Paths {
		st.Paths[key] = v
	}
	return saveState(db, st)
}

// resetState clears all persisted push state from the database. A nil db is a no-op.
func resetState(db *sql.DB) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM traffic`); err != nil {
		return fmt.Errorf("clear traffic: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM snapshots`); err != nil {
		return fmt.Errorf("clear snapshots: %w", err)
	}
	return tx.Commit()
}

// splitKey splits a "repo|date" key. repo is "owner/repo" (never contains "|").
func splitKey(key string) (repo, date string, ok bool) {
	return strings.Cut(key, "|")
}

// buildEvents converts ghtraffic records into Umami events, emitting only the
// delta since the last push for each repo+date. Returns the events to send and
// an updated state to persist after a successful send.
func buildEvents(records []Record, websiteID string, st pushState, now time.Time) ([]umamiEvent, pushState) {
	today := now.UTC().Format("2006-01-02")

	next := newPushState()
	for k, v := range st.Traffic {
		next.Traffic[k] = v
	}
	for k, v := range st.Referrers {
		next.Referrers[k] = v
	}
	for k, v := range st.Paths {
		next.Paths[k] = v
	}

	latestByRepo := make(map[string]Record)
	for _, r := range records {
		if prev, ok := latestByRepo[r.Repo]; !ok || r.Date > prev.Date {
			latestByRepo[r.Repo] = r
		}
	}

	var events []umamiEvent

	for _, r := range records {
		key := r.Repo + "|" + r.Date
		prev := next.Traffic[key]

		viewDelta := r.Views.Count - prev.Views
		cloneDelta := r.Clones.Count - prev.Clones
		if viewDelta <= 0 && cloneDelta <= 0 {
			continue
		}

		ts := dayTimestamp(r.Date, r.CollectedAt, today)

		if viewDelta > 0 {
			// No Name → Umami v2 treats this as a pageview (shows in main chart).
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				Screen: "1920x1080", Language: "en-US",
				URL: "/" + r.Repo, Timestamp: ts,
			}}
			events = append(events, repeatEvent(e, viewDelta)...)
		}
		if cloneDelta > 0 {
			// No Name → Umami treats this as a pageview, so clones count as
			// unique visitors. URL prefix /clone/ distinguishes them from views.
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				Screen: "1920x1080", Language: "en-US",
				URL: "/clone/" + r.Repo, Timestamp: ts,
			}}
			events = append(events, repeatEvent(e, cloneDelta)...)
		}

		next.Traffic[key] = trafficCounts{
			Views:  max(r.Views.Count, prev.Views),
			Clones: max(r.Clones.Count, prev.Clones),
		}
	}

	for repo, r := range latestByRepo {
		collectedAt, err := time.Parse(time.RFC3339, r.CollectedAt)
		if err != nil {
			collectedAt = now
		}
		collectedDate := collectedAt.UTC().Format("2006-01-02")

		refKey := repo + "|" + collectedDate
		if !next.Referrers[refKey] && len(r.Referrers) > 0 {
			events = append(events, referrerEvents(repo, r.Referrers, collectedAt, websiteID)...)
			next.Referrers[refKey] = true
		}

		pathKey := repo + "|" + collectedDate
		if !next.Paths[pathKey] && len(r.Paths) > 0 {
			events = append(events, pathEvents(repo, r.Paths, collectedAt, websiteID)...)
			next.Paths[pathKey] = true
		}
	}

	return events, next
}

// dayTimestamp returns the Umami event timestamp for a record in Unix seconds.
// Umami's /api/send endpoint uses Unix seconds for the timestamp field.
// Historical dates use UTC midnight; today's date uses CollectedAt.
func dayTimestamp(date, collectedAt, today string) int64 {
	if date == today {
		if t, err := time.Parse(time.RFC3339, collectedAt); err == nil {
			return t.UTC().Unix()
		}
	}
	t, _ := time.Parse("2006-01-02", date)
	return t.UTC().Unix()
}

// referrerScreen returns a deterministic screen resolution derived from the
// referrer name. Umami's session is keyed on IP+UA+hostname+language+screen,
// so giving each referrer source a unique screen forces a distinct session and
// ensures all referrers appear separately in the dashboard.
func referrerScreen(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("1920x%d", 900+h.Sum32()%1000)
}

// referrerEvents builds one pageview per referral hit with the Referrer
// payload field set so Umami populates its built-in referrer breakdown.
func referrerEvents(repo string, refs []Referrer, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	for _, ref := range refs {
		referrerURL := ref.Name
		if !strings.Contains(ref.Name, "://") {
			referrerURL = "https://" + ref.Name
		}
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "github.com",
			Screen: referrerScreen(ref.Name), Language: "en-US",
			URL: "/" + repo, Referrer: referrerURL,
			Timestamp: ts,
		}}
		out = append(out, repeatEvent(e, ref.Count)...)
	}
	return out
}

// pathEvents builds one pageview per path hit using the GitHub path as the URL.
// This populates Umami's top-pages breakdown with the actual repo subpaths.
// No Name → Umami v2 treats events without a name as pageviews.
func pathEvents(_ string, paths []Path, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	for _, p := range paths {
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "github.com",
			Screen: "1920x1080", Language: "en-US",
			URL: p.Path, Timestamp: ts,
		}}
		out = append(out, repeatEvent(e, p.Count)...)
	}
	return out
}

// repeatEvent returns a slice of n copies of e. Returns nil if n <= 0.
func repeatEvent(e umamiEvent, n int) []umamiEvent {
	if n <= 0 {
		return nil
	}
	out := make([]umamiEvent, n)
	for i := range out {
		out[i] = e
	}
	return out
}

// pusher sends Umami events over HTTP to /api/send one at a time.
type pusher struct {
	httpClient *http.Client
	baseURL    string
	batchSize  int // reserved for future rate-limiting; unused currently
}

func (p *pusher) pushAll(events []umamiEvent) error {
	for i, e := range events {
		if err := p.sendEvent(e); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	return nil
}

func (p *pusher) sendEvent(e umamiEvent) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/api/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
