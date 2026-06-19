package main

import (
	"os"
	"testing"
)

// TestPGStore_RoundTrip exercises the Postgres backend against a real database.
// It is skipped unless GHPUSH_TEST_PG_DSN points at a throwaway Postgres (the
// test writes into and clears the "ghpush" schema), so CI without a database
// skips it rather than failing.
func TestPGStore_RoundTrip(t *testing.T) {
	dsn := os.Getenv("GHPUSH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set GHPUSH_TEST_PG_DSN to run the Postgres integration test")
	}

	store, err := newPGStore(dsn)
	if err != nil {
		t.Fatalf("newPGStore: %v", err)
	}
	defer store.close() //nolint:errcheck

	// Start clean and clean up afterwards so a shared DB isn't polluted.
	if err := store.reset(); err != nil {
		t.Fatalf("reset (pre): %v", err)
	}
	t.Cleanup(func() { _ = store.reset() })

	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 11, Clones: 4}
	st.Referrers["a/b|2026-02-21"] = true
	st.Paths["a/b|2026-02-21"] = true
	if err := store.save(st); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Re-save with higher counts: traffic upserts, snapshots ON CONFLICT DO
	// NOTHING. Must not error and must reflect the new counts.
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 20, Clones: 6}
	if err := store.save(st); err != nil {
		t.Fatalf("save 2 (upsert): %v", err)
	}

	loaded, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tc := loaded.Traffic["a/b|2026-02-21"]
	if tc.Views != 20 || tc.Clones != 6 {
		t.Errorf("traffic = %+v, want {Views:20 Clones:6}", tc)
	}
	if !loaded.Referrers["a/b|2026-02-21"] || !loaded.Paths["a/b|2026-02-21"] {
		t.Error("expected referrer and path snapshots present")
	}

	if err := store.reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	loaded, err = store.load()
	if err != nil {
		t.Fatalf("load after reset: %v", err)
	}
	if len(loaded.Traffic) != 0 || len(loaded.Referrers) != 0 || len(loaded.Paths) != 0 {
		t.Errorf("expected empty state after reset, got %+v", loaded)
	}
}
