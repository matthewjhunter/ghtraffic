package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// pushState is the in-memory representation of what has already been pushed.
type pushState struct {
	// Traffic maps "repo|date" to cumulative counts already pushed.
	Traffic map[string]trafficCounts
	// Referrers maps "repo|referrer" and Paths maps "repo|path" to the
	// cumulative hit count already pushed. GitHub reports both as running
	// 14-day totals, so only the growth since the last push is new traffic.
	Referrers map[string]int
	Paths     map[string]int
}

type trafficCounts struct {
	Views  int
	Clones int
}

func newPushState() pushState {
	return pushState{
		Traffic:   make(map[string]trafficCounts),
		Referrers: make(map[string]int),
		Paths:     make(map[string]int),
	}
}

// stateStore persists ghpush's record of what has already been sent to Umami.
// Two backends implement it: sqliteStore for standalone/local use and pgStore
// for the containerized deployment that shares the central Postgres. nopStore
// is used when no persistence is configured (e.g. dry runs), in which case
// every record looks new.
type stateStore interface {
	// load reads the full push state into memory. An empty store returns an
	// empty (non-nil) state.
	load() (pushState, error)
	// save persists the push state. Traffic and snapshot counts are both
	// upserted, so save is idempotent.
	save(st pushState) error
	// reset clears all persisted state.
	reset() error
	// close releases the store's resources.
	close() error
}

// nopStore is a stateStore that persists nothing. load always returns an empty
// state, so callers treat all records as new.
type nopStore struct{}

func (nopStore) load() (pushState, error) { return newPushState(), nil }
func (nopStore) save(pushState) error     { return nil }
func (nopStore) reset() error             { return nil }
func (nopStore) close() error             { return nil }

// sqliteStore persists push state in a SQLite database. Pass ":memory:" for an
// ephemeral in-process database (useful in tests).
type sqliteStore struct {
	db *sql.DB
}

// newSQLiteStore opens (or creates) the SQLite database at path and initialises
// the schema.
func newSQLiteStore(path string) (*sqliteStore, error) {
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
		CREATE TABLE IF NOT EXISTS snapshot_counts (
			repo  TEXT NOT NULL,
			kind  TEXT NOT NULL,
			item  TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, kind, item)
		);
		-- Legacy per-day "already sent" flags, replaced by snapshot_counts.
		-- Their date-keyed rows carry no count, so there is nothing to migrate.
		DROP TABLE IF EXISTS snapshots;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

// newSQLiteStoreReadOnly opens the SQLite database at path read-only. Unlike
// newSQLiteStore it sets no WAL pragma and creates no tables (both are writes),
// so it works against a read-only file or bind mount. Only load() is meaningful;
// save/reset will fail. Used as the source for -migrate-sqlite, which must not
// mutate the file it copies from.
func newSQLiteStoreReadOnly(path string) (*sqliteStore, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) close() error { return s.db.Close() }

func (s *sqliteStore) load() (pushState, error) { //nolint:dupl // mirrors pgStore.load/save; SQL dialect differs enough (placeholder syntax, upsert clause) that sharing an implementation needs a query-builder abstraction not worth it for two backends
	st := newPushState()

	rows, err := s.db.Query(`SELECT repo, date, views, clones FROM traffic`)
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

	rows2, err := s.db.Query(`SELECT repo, kind, item, count FROM snapshot_counts`)
	if err != nil {
		return st, fmt.Errorf("query snapshot counts: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var repo, kind, item string
		var count int
		if err := rows2.Scan(&repo, &kind, &item, &count); err != nil {
			return st, err
		}
		key := repo + "|" + item
		switch kind {
		case "referrer":
			st.Referrers[key] = count
		case "path":
			st.Paths[key] = count
		}
	}
	return st, rows2.Err()
}

func (s *sqliteStore) save(st pushState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if the tx already committed; nothing actionable if the rollback itself fails

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

	for _, s := range snapshotKinds(st) {
		for key, count := range s.counts {
			repo, item, ok := splitKey(key)
			if !ok {
				continue
			}
			_, err := tx.Exec(`
				INSERT INTO snapshot_counts (repo, kind, item, count) VALUES (?, ?, ?, ?)
				ON CONFLICT (repo, kind, item) DO UPDATE SET count = excluded.count
			`, repo, s.kind, item, count)
			if err != nil {
				return fmt.Errorf("upsert %s snapshot %s: %w", s.kind, key, err)
			}
		}
	}

	return tx.Commit()
}

// snapshotKind pairs a snapshot map with the kind column value the stores
// persist it under.
type snapshotKind struct {
	kind   string
	counts map[string]int
}

// snapshotKinds lists st's snapshot maps so both backends write them in one loop.
func snapshotKinds(st pushState) []snapshotKind {
	return []snapshotKind{
		{"referrer", st.Referrers},
		{"path", st.Paths},
	}
}

func (s *sqliteStore) reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op if the tx already committed; nothing actionable if the rollback itself fails
	if _, err := tx.Exec(`DELETE FROM traffic`); err != nil {
		return fmt.Errorf("clear traffic: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM snapshot_counts`); err != nil {
		return fmt.Errorf("clear snapshot counts: %w", err)
	}
	return tx.Commit()
}

// jsonPushState mirrors the legacy JSON state file format used before the
// SQLite migration. Field names match the original json tags exactly. Its
// referrer and path entries were per-day "already sent" flags keyed by
// collected date, carrying no count, so they are decoded but not imported.
type jsonPushState struct {
	Traffic map[string]struct {
		Views  int `json:"views"`
		Clones int `json:"clones"`
	} `json:"traffic"`
}

// importState reads a legacy JSON state file and merges its contents into the
// store. Existing rows are updated via the store's upsert, so the import is
// idempotent.
func importState(path string, store stateStore) error {
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
	return store.save(st)
}

// copyState loads the full state from src and writes it into dst. It is used to
// migrate an existing SQLite state file into Postgres without re-pushing data
// that Umami already holds.
func copyState(src, dst stateStore) error {
	st, err := src.load()
	if err != nil {
		return fmt.Errorf("load source: %w", err)
	}
	if err := dst.save(st); err != nil {
		return fmt.Errorf("save destination: %w", err)
	}
	return nil
}

// splitKey splits a "repo|suffix" key, where suffix is a date, a path, or a
// referrer name. repo is "owner/repo" and never contains "|", so cutting at the
// first separator is correct even when the suffix contains one.
func splitKey(key string) (repo, suffix string, ok bool) {
	return strings.Cut(key, "|")
}
