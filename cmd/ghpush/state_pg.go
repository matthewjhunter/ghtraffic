package main

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgStore persists push state in the central Postgres. Its tables live in a
// dedicated "ghpush" schema so they never collide with the raw traffic-history
// tables the collector writes into the same database.
type pgStore struct {
	db *sql.DB
}

// newPGStore connects to Postgres using a standard libpq-style DSN or URL
// (e.g. "postgres://user:pass@host:5432/ghtraffic") and ensures the schema.
func newPGStore(dsn string) (*pgStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	s := &pgStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *pgStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE SCHEMA IF NOT EXISTS ghpush;
		CREATE TABLE IF NOT EXISTS ghpush.traffic (
			repo   text    NOT NULL,
			date   text    NOT NULL,
			views  integer NOT NULL DEFAULT 0,
			clones integer NOT NULL DEFAULT 0,
			PRIMARY KEY (repo, date)
		);
		CREATE TABLE IF NOT EXISTS ghpush.snapshots (
			repo           text NOT NULL,
			kind           text NOT NULL,
			collected_date text NOT NULL,
			PRIMARY KEY (repo, kind, collected_date)
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *pgStore) close() error { return s.db.Close() }

func (s *pgStore) load() (pushState, error) {
	st := newPushState()

	rows, err := s.db.Query(`SELECT repo, date, views, clones FROM ghpush.traffic`)
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

	rows2, err := s.db.Query(`SELECT repo, kind, collected_date FROM ghpush.snapshots`)
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

func (s *pgStore) save(st pushState) error {
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
			INSERT INTO ghpush.traffic (repo, date, views, clones) VALUES ($1, $2, $3, $4)
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
			`INSERT INTO ghpush.snapshots (repo, kind, collected_date) VALUES ($1, 'referrer', $2)
			 ON CONFLICT (repo, kind, collected_date) DO NOTHING`,
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
			`INSERT INTO ghpush.snapshots (repo, kind, collected_date) VALUES ($1, 'path', $2)
			 ON CONFLICT (repo, kind, collected_date) DO NOTHING`,
			repo, date,
		); err != nil {
			return fmt.Errorf("insert path snapshot %s: %w", key, err)
		}
	}

	return tx.Commit()
}

func (s *pgStore) reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM ghpush.traffic`); err != nil {
		return fmt.Errorf("clear traffic: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM ghpush.snapshots`); err != nil {
		return fmt.Errorf("clear snapshots: %w", err)
	}
	return tx.Commit()
}
