// ghpush reads ghtraffic NDJSON from stdin and pushes the records to Umami
// as custom events, using the /api/batch endpoint with historical timestamps.
//
// Requires Umami v2.17 or later (adds /api/batch and timestamp support).
//
// Usage:
//
//	ghtraffic -seen traffic.jsonl >> traffic.jsonl
//	ghpush -pushed pushed.db < traffic.jsonl
//
// Use -init to bootstrap a fresh Umami site from all stored historical data,
// ignoring any prior push state and resetting it afterwards.
//
// Environment variables:
//
//	UMAMI_URL        Base URL of your Umami instance (e.g. https://umami.example.com)
//	UMAMI_WEBSITE_ID Website UUID from Umami settings
package main

import (
	"bufio"
	"database/sql"
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

	umamiURL := flag.String("url", envOrDefault("UMAMI_URL", ""), "Umami base URL (e.g. https://umami.example.com)")
	websiteID := flag.String("website", envOrDefault("UMAMI_WEBSITE_ID", ""), "Umami website UUID")
	pushedFile := flag.String("pushed", "", "SQLite state file tracking pushed counts (prevents re-pushing on re-run)")
	batchSize := flag.Int("batch-size", 100, "events per POST to Umami /api/batch")
	dryRun := flag.Bool("dry-run", false, "print events as JSON to stdout without sending")
	init_ := flag.Bool("init", false, "bootstrap from scratch: ignore push state and push all historical data")
	importJSON := flag.String("import-json", "", "import a legacy JSON state file into the SQLite DB and exit")
	flag.Parse()

	if !*dryRun {
		if *umamiURL == "" {
			log.Fatal("no Umami URL: set UMAMI_URL or use -url")
		}
		if *websiteID == "" {
			log.Fatal("no Umami website ID: set UMAMI_WEBSITE_ID or use -website")
		}
	}

	var db *sql.DB
	if *pushedFile != "" {
		var err error
		db, err = openDB(*pushedFile)
		if err != nil {
			log.Fatalf("open state db: %v", err)
		}
		defer db.Close()
	}

	// -import-json: migrate legacy JSON state into the SQLite DB and exit.
	if *importJSON != "" {
		if db == nil {
			log.Fatal("-import-json requires -pushed to specify the SQLite database")
		}
		if err := importJSONState(*importJSON, db); err != nil {
			log.Fatalf("import: %v", err)
		}
		log.Print("import complete")
		return
	}

	// -init: treat all records as new regardless of stored state.
	var st pushState
	if *init_ {
		st = newPushState()
		log.Print("init mode: ignoring existing push state, all records will be sent")
	} else {
		var err error
		st, err = loadState(db)
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
		batchSize:  *batchSize,
	}
	if err := p.pushAll(events); err != nil {
		log.Fatalf("push: %v", err)
	}

	// On -init, clear stale state before writing the fresh baseline.
	if *init_ {
		if err := resetState(db); err != nil {
			log.Printf("warning: could not reset state db: %v", err)
		}
	}

	if err := saveState(db, newSt); err != nil {
		// Non-fatal: push succeeded; warn so the user can investigate.
		log.Printf("warning: could not save state, next run may re-push: %v", err)
	}

	log.Print("done")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
