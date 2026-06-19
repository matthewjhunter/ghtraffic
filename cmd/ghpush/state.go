package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// pushState is the in-memory representation of what has already been pushed.
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

// stateStore persists ghpush's record of what has already been sent to Umami.
// Two backends implement it: sqliteStore for standalone/local use and pgStore
// for the containerized deployment that shares the central Postgres. nopStore
// is used when no persistence is configured (e.g. dry runs), in which case
// every record looks new.
type stateStore interface {
	// load reads the full push state into memory. An empty store returns an
	// empty (non-nil) state.
	load() (pushState, error)
	// save persists the push state. Traffic counts are upserted; snapshot
	// flags are inserted-or-ignored, so save is idempotent.
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
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) close() error { return s.db.Close() }

func (s *sqliteStore) load() (pushState, error) {
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

	rows2, err := s.db.Query(`SELECT repo, kind, collected_date FROM snapshots`)
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

func (s *sqliteStore) save(st pushState) error {
	tx, err := s.db.Begin()
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

func (s *sqliteStore) reset() error {
	tx, err := s.db.Begin()
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
	maps.Copy(st.Referrers, js.Referrers)
	maps.Copy(st.Paths, js.Paths)
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

// splitKey splits a "repo|date" key. repo is "owner/repo" (never contains "|").
func splitKey(key string) (repo, date string, ok bool) {
	return strings.Cut(key, "|")
}
