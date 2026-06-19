// ghpush reads ghtraffic NDJSON from stdin and pushes the records to Umami
// as custom events, using the /api/send endpoint with historical timestamps.
//
// Requires Umami v2 or later.
//
// Usage:
//
//	ghtraffic -seen traffic.jsonl >> traffic.jsonl
//	ghpush -pushed pushed.db < traffic.jsonl                  # SQLite state
//	ghpush -pg "$GHPUSH_DATABASE_URL" < traffic.jsonl         # Postgres state
//
// Use -init to bootstrap a fresh Umami site from all stored historical data,
// ignoring any prior push state and resetting it afterwards.
//
// Use -migrate-sqlite to copy an existing SQLite state file into the Postgres
// store (target set via -pg / GHPUSH_DATABASE_URL) without re-pushing data.
//
// Environment variables:
//
//	UMAMI_URL           Base URL of your Umami instance (e.g. http://umami:3000)
//	UMAMI_WEBSITE_ID    Website UUID from Umami settings
//	GHPUSH_DATABASE_URL Postgres DSN/URL for the state store (alternative to -pg)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	umamiURL := flag.String("url", envOrDefault("UMAMI_URL", ""), "Umami base URL (e.g. http://umami:3000)")
	websiteID := flag.String("website", envOrDefault("UMAMI_WEBSITE_ID", ""), "Umami website UUID")
	pushedFile := flag.String("pushed", "", "SQLite state file tracking pushed counts (prevents re-pushing on re-run)")
	pgDSN := flag.String("pg", envOrDefault("GHPUSH_DATABASE_URL", ""), "Postgres DSN/URL for the state store (alternative to -pushed)")
	dryRun := flag.Bool("dry-run", false, "print events as JSON to stdout without sending")
	init_ := flag.Bool("init", false, "bootstrap from scratch: ignore push state and push all historical data")
	importJSON := flag.String("import-json", "", "import a legacy JSON state file into the state store and exit")
	migrateSQLite := flag.String("migrate-sqlite", "", "copy an existing SQLite state file into the Postgres store and exit")
	flag.Parse()

	if *pushedFile != "" && *pgDSN != "" {
		log.Fatal("use only one of -pushed (SQLite) or -pg (Postgres)")
	}

	store, err := openStore(*pgDSN, *pushedFile)
	if err != nil {
		log.Fatalf("open state store: %v", err)
	}
	defer store.close() //nolint:errcheck

	// -migrate-sqlite: copy an existing SQLite state file into the configured
	// store (intended to seed Postgres from the legacy workstation pushed.db).
	if *migrateSQLite != "" {
		if *pgDSN == "" {
			log.Fatal("-migrate-sqlite requires -pg (or GHPUSH_DATABASE_URL) as the destination")
		}
		src, err := newSQLiteStoreReadOnly(*migrateSQLite)
		if err != nil {
			log.Fatalf("open source SQLite state: %v", err)
		}
		defer src.close() //nolint:errcheck
		if err := copyState(src, store); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Print("migrate complete")
		return
	}

	// -import-json: migrate legacy JSON state into the store and exit.
	if *importJSON != "" {
		if _, ok := store.(nopStore); ok {
			log.Fatal("-import-json requires -pushed or -pg to specify the state store")
		}
		if err := importState(*importJSON, store); err != nil {
			log.Fatalf("import: %v", err)
		}
		log.Print("import complete")
		return
	}

	// Past this point we actually push, which requires Umami config (unless
	// -dry-run). The migrate/import paths above intentionally do not need it.
	if !*dryRun {
		if *umamiURL == "" {
			log.Fatal("no Umami URL: set UMAMI_URL or use -url")
		}
		if *websiteID == "" {
			log.Fatal("no Umami website ID: set UMAMI_WEBSITE_ID or use -website")
		}
	}

	// -init: treat all records as new regardless of stored state.
	var st pushState
	if *init_ {
		st = newPushState()
		log.Print("init mode: ignoring existing push state, all records will be sent")
	} else {
		st, err = store.load()
		if err != nil {
			log.Fatalf("load state: %v", err)
		}
	}

	var records []Record
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var r Record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			log.Printf("skip malformed line: %v", err)
			continue
		}
		if r.Repo == "" || r.Date == "" {
			continue
		}
		records = append(records, r)
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("read stdin: %v", err)
	}

	events, newSt := buildEvents(records, *websiteID, st, time.Now())
	if len(events) == 0 {
		log.Print("no new events to push")
		return
	}
	log.Printf("pushing %d events to Umami", len(events))

	if *dryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				log.Fatalf("encode: %v", err)
			}
		}
		return
	}

	p := &pusher{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    *umamiURL,
	}
	if err := p.pushAll(events); err != nil {
		log.Fatalf("push: %v", err)
	}

	// On -init, clear stale state before writing the fresh baseline.
	if *init_ {
		if err := store.reset(); err != nil {
			log.Printf("warning: could not reset state store: %v", err)
		}
	}

	if err := store.save(newSt); err != nil {
		// Non-fatal: push succeeded; warn so the user can investigate.
		log.Printf("warning: could not save state, next run may re-push: %v", err)
	}

	log.Print("done")
}

// openStore selects the state backend: Postgres when a DSN is given, otherwise
// SQLite when a file is given, otherwise a no-op store that persists nothing.
func openStore(pgDSN, sqlitePath string) (stateStore, error) {
	switch {
	case pgDSN != "":
		return newPGStore(pgDSN)
	case sqlitePath != "":
		return newSQLiteStore(sqlitePath)
	default:
		return nopStore{}, nil
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
