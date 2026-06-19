package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// --- repeatEvent ---

func TestRepeatEvent_ZeroCount(t *testing.T) {
	if got := repeatEvent(umamiEvent{Type: "event"}, 0); got != nil {
		t.Errorf("expected nil for count=0, got %v", got)
	}
}

func TestRepeatEvent_NegativeCount(t *testing.T) {
	if got := repeatEvent(umamiEvent{Type: "event"}, -1); got != nil {
		t.Errorf("expected nil for count=-1, got %v", got)
	}
}

func TestRepeatEvent_Count(t *testing.T) {
	e := umamiEvent{Type: "event", Payload: eventPayload{Name: "github_view"}}
	got := repeatEvent(e, 5)
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i, ev := range got {
		if ev.Payload.Name != "github_view" {
			t.Errorf("[%d] Name = %q, want github_view", i, ev.Payload.Name)
		}
	}
}

// --- dayTimestamp ---

func TestDayTimestamp_Historical(t *testing.T) {
	ts := dayTimestamp("2026-02-15", "2026-02-21T09:00:00Z", "2026-02-21")
	expected, _ := time.Parse("2006-01-02", "2026-02-15")
	if ts != expected.UTC().Unix() {
		t.Errorf("historical timestamp = %d, want %d (UTC midnight 2026-02-15)", ts, expected.UTC().Unix())
	}
}

func TestDayTimestamp_Today(t *testing.T) {
	ts := dayTimestamp("2026-02-21", "2026-02-21T09:30:00Z", "2026-02-21")
	expected, _ := time.Parse(time.RFC3339, "2026-02-21T09:30:00Z")
	if ts != expected.UTC().Unix() {
		t.Errorf("today timestamp = %d, want %d (CollectedAt)", ts, expected.UTC().Unix())
	}
}

func TestDayTimestamp_TodayBadCollectedAt(t *testing.T) {
	// Falls back to midnight when CollectedAt is unparseable.
	ts := dayTimestamp("2026-02-21", "not-a-timestamp", "2026-02-21")
	expected, _ := time.Parse("2006-01-02", "2026-02-21")
	if ts != expected.UTC().Unix() {
		t.Errorf("fallback timestamp = %d, want %d (UTC midnight)", ts, expected.UTC().Unix())
	}
}

// --- buildEvents: traffic deltas ---

func TestBuildEvents_FirstRun_FullCount(t *testing.T) {
	records := []Record{
		{
			Repo:        "a/b",
			Date:        "2026-02-15",
			CollectedAt: "2026-02-21T09:00:00Z",
			Views:       DayCounts{Count: 10},
			Clones:      DayCounts{Count: 30, Uniques: 3},
		},
	}
	now := time.Date(2026, 2, 21, 9, 0, 0, 0, time.UTC)
	events, newSt := buildEvents(records, "uuid", newPushState(), now)

	var views, clones int
	for _, e := range events {
		switch e.Payload.URL {
		case "/a/b":
			views++
		case "/clone/a/b":
			clones++
		}
	}
	if views != 10 {
		t.Errorf("views = %d, want 10 (full count on first run)", views)
	}
	if clones != 3 {
		t.Errorf("clones = %d, want 3 (uniques, not total count)", clones)
	}
	if newSt.Traffic["a/b|2026-02-15"].Views != 10 {
		t.Errorf("state views = %d, want 10", newSt.Traffic["a/b|2026-02-15"].Views)
	}
}

func TestBuildEvents_Delta(t *testing.T) {
	// Simulate a second hourly run where counts grew.
	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 10, Clones: 3}

	records := []Record{
		{
			Repo:        "a/b",
			Date:        "2026-02-21",
			CollectedAt: "2026-02-21T10:00:00Z",
			Views:       DayCounts{Count: 15},
			Clones:      DayCounts{Count: 50, Uniques: 5},
		},
	}
	now := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	events, newSt := buildEvents(records, "uuid", st, now)

	var views, clones int
	for _, e := range events {
		switch e.Payload.URL {
		case "/a/b":
			views++
		case "/clone/a/b":
			clones++
		}
	}
	if views != 5 { // 15 - 10
		t.Errorf("views = %d, want 5 (delta)", views)
	}
	if clones != 2 { // 5 uniques - 3
		t.Errorf("clones = %d, want 2 (uniques delta)", clones)
	}
	if newSt.Traffic["a/b|2026-02-21"].Views != 15 {
		t.Errorf("state views = %d, want 15", newSt.Traffic["a/b|2026-02-21"].Views)
	}
}

func TestBuildEvents_NoNewTraffic(t *testing.T) {
	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 10, Clones: 3}

	records := []Record{
		{
			Repo:        "a/b",
			Date:        "2026-02-21",
			CollectedAt: "2026-02-21T10:00:00Z",
			Views:       DayCounts{Count: 10},             // same as state
			Clones:      DayCounts{Count: 30, Uniques: 3}, // uniques same as state
		},
	}
	now := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	events, _ := buildEvents(records, "uuid", st, now)

	for _, e := range events {
		if e.Payload.URL == "/a/b" || e.Payload.URL == "/clone/a/b" {
			t.Errorf("unexpected traffic event (url=%q) when counts unchanged", e.Payload.URL)
		}
	}
}

func TestBuildEvents_TodayUsesCollectedAtTimestamp(t *testing.T) {
	records := []Record{
		{
			Repo:        "a/b",
			Date:        "2026-02-21",
			CollectedAt: "2026-02-21T14:30:00Z",
			Views:       DayCounts{Count: 5},
		},
	}
	now := time.Date(2026, 2, 21, 14, 30, 0, 0, time.UTC)
	events, _ := buildEvents(records, "uuid", newPushState(), now)

	expected, _ := time.Parse(time.RFC3339, "2026-02-21T14:30:00Z")
	for _, e := range events {
		if e.Type == "event" && e.Payload.Name == "" && e.Payload.Timestamp != expected.UTC().Unix() {
			t.Errorf("today timestamp = %d, want %d (CollectedAt)", e.Payload.Timestamp, expected.UTC().Unix())
		}
	}
}

func TestBuildEvents_HistoricalUsesMidnightTimestamp(t *testing.T) {
	records := []Record{
		{
			Repo:        "a/b",
			Date:        "2026-02-15",
			CollectedAt: "2026-02-21T09:00:00Z",
			Views:       DayCounts{Count: 5},
		},
	}
	now := time.Date(2026, 2, 21, 9, 0, 0, 0, time.UTC)
	events, _ := buildEvents(records, "uuid", newPushState(), now)

	midnight, _ := time.Parse("2006-01-02", "2026-02-15")
	for _, e := range events {
		if e.Type == "event" && e.Payload.Name == "" && e.Payload.Timestamp != midnight.UTC().Unix() {
			t.Errorf("historical timestamp = %d, want %d (midnight)", e.Payload.Timestamp, midnight.UTC().Unix())
		}
	}
}

func TestBuildEvents_StateNotMutated(t *testing.T) {
	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 5, Clones: 1}

	records := []Record{
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T10:00:00Z", Views: DayCounts{Count: 10}},
	}
	now := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	_, _ = buildEvents(records, "uuid", st, now)

	// Input state must be unchanged.
	if st.Traffic["a/b|2026-02-21"].Views != 5 {
		t.Errorf("input state was mutated: views = %d, want 5", st.Traffic["a/b|2026-02-21"].Views)
	}
}

func TestBuildEvents_ReferrersFromLatestRecord(t *testing.T) {
	older := Record{
		Repo: "a/b", Date: "2026-02-19", CollectedAt: "2026-02-20T00:00:00Z",
		Referrers: []Referrer{{Name: "old.com", Count: 2, Uniques: 2}},
	}
	newer := Record{
		Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z",
		Referrers: []Referrer{{Name: "new.com", Count: 5, Uniques: 3}},
	}
	now := time.Date(2026, 2, 21, 0, 0, 0, 0, time.UTC)
	events, _ := buildEvents([]Record{older, newer}, "uuid", newPushState(), now)

	var refCount int
	for _, e := range events {
		if e.Payload.Referrer != "" {
			refCount++
			if e.Payload.Referrer != "https://new.com" {
				t.Errorf("Referrer = %q, want https://new.com", e.Payload.Referrer)
			}
		}
	}
	if refCount != 5 {
		t.Errorf("referrer event count = %d, want 5 (from new.com)", refCount)
	}
}

func TestBuildEvents_SkipsPushedReferrers(t *testing.T) {
	st := newPushState()
	st.Referrers["a/b|2026-02-21"] = true

	r := Record{
		Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z",
		Referrers: []Referrer{{Name: "google.com", Count: 5, Uniques: 3}},
	}
	now := time.Date(2026, 2, 21, 0, 0, 0, 0, time.UTC)
	events, _ := buildEvents([]Record{r}, "uuid", st, now)

	for _, e := range events {
		if e.Payload.Referrer != "" {
			t.Error("expected no referrer events when already in state")
		}
	}
}

// --- referrerEvents ---

func TestReferrerEvents_Count(t *testing.T) {
	refs := []Referrer{
		{Name: "google.com", Count: 10, Uniques: 8},
		{Name: "github.com", Count: 5, Uniques: 4},
	}
	ts := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)
	events := referrerEvents("owner/repo", refs, ts, "uuid")

	if len(events) != 15 {
		t.Fatalf("len(events) = %d, want 15 (10+5)", len(events))
	}
	if events[0].Payload.Referrer != "https://google.com" {
		t.Errorf("events[0] Referrer = %q, want https://google.com", events[0].Payload.Referrer)
	}
	if events[10].Payload.Referrer != "https://github.com" {
		t.Errorf("events[10] Referrer = %q, want https://github.com", events[10].Payload.Referrer)
	}
}

func TestReferrerEvents_ZeroCount(t *testing.T) {
	refs := []Referrer{{Name: "google.com", Count: 0}}
	if got := referrerEvents("a/b", refs, time.Now(), "uuid"); len(got) != 0 {
		t.Errorf("expected 0 events for count=0, got %d", len(got))
	}
}

// --- pathEvents ---

func TestPathEvents_Count(t *testing.T) {
	paths := []Path{
		{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 20},
		{Path: "/owner/repo/blob/main/main.go", Title: "main.go", Count: 5},
	}
	events := pathEvents("owner/repo", paths, time.Now(), "uuid")

	if len(events) != 25 {
		t.Fatalf("len(events) = %d, want 25 (20+5)", len(events))
	}
}

func TestPathEvents_URLIsPath(t *testing.T) {
	paths := []Path{{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 1}}
	events := pathEvents("owner/repo", paths, time.Now(), "uuid")
	if len(events) == 0 {
		t.Fatal("expected events, got none")
	}
	if events[0].Payload.URL != "/owner/repo/blob/main/README.md" {
		t.Errorf("URL = %q, want /owner/repo/blob/main/README.md", events[0].Payload.URL)
	}
}

// --- stateStore: load / save / reset ---

func newTestSQLiteStore(t *testing.T) *sqliteStore {
	t.Helper()
	s, err := newSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("newSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func TestNopStore_LoadEmpty(t *testing.T) {
	st, err := nopStore{}.load()
	if err != nil {
		t.Fatalf("nopStore.load: %v", err)
	}
	if len(st.Traffic) != 0 || len(st.Referrers) != 0 || len(st.Paths) != 0 {
		t.Errorf("expected empty state from nopStore, got %+v", st)
	}
}

func TestNopStore_SaveResetNoop(t *testing.T) {
	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 1}
	if err := (nopStore{}).save(st); err != nil {
		t.Errorf("nopStore.save should be a no-op, got: %v", err)
	}
	if err := (nopStore{}).reset(); err != nil {
		t.Errorf("nopStore.reset should be a no-op, got: %v", err)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	store := newTestSQLiteStore(t)

	original := newPushState()
	original.Traffic["owner/repo|2026-02-21"] = trafficCounts{Views: 42, Clones: 8}
	original.Referrers["owner/repo|2026-02-21"] = true
	original.Paths["owner/repo|2026-02-21"] = true

	if err := store.save(original); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	tc := loaded.Traffic["owner/repo|2026-02-21"]
	if tc.Views != 42 || tc.Clones != 8 {
		t.Errorf("traffic = %+v, want {Views:42 Clones:8}", tc)
	}
	if !loaded.Referrers["owner/repo|2026-02-21"] {
		t.Error("expected referrer key to be present")
	}
	if !loaded.Paths["owner/repo|2026-02-21"] {
		t.Error("expected path key to be present")
	}
}

func TestSaveState_TrafficUpsert(t *testing.T) {
	store := newTestSQLiteStore(t)

	st1 := newPushState()
	st1.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 10, Clones: 2}
	if err := store.save(st1); err != nil {
		t.Fatalf("save 1: %v", err)
	}

	st2 := newPushState()
	st2.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 15, Clones: 5}
	if err := store.save(st2); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	loaded, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tc := loaded.Traffic["a/b|2026-02-21"]
	if tc.Views != 15 || tc.Clones != 5 {
		t.Errorf("after upsert traffic = %+v, want {Views:15 Clones:5}", tc)
	}
}

func TestSaveState_SnapshotIdempotent(t *testing.T) {
	store := newTestSQLiteStore(t)

	st := newPushState()
	st.Referrers["a/b|2026-02-21"] = true
	st.Paths["a/b|2026-02-21"] = true

	// Saving twice must not fail (insert-or-ignore semantics).
	if err := store.save(st); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := store.save(st); err != nil {
		t.Fatalf("save 2 (duplicate): %v", err)
	}
}

func TestResetState_ClearsAllTables(t *testing.T) {
	store := newTestSQLiteStore(t)

	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 10, Clones: 2}
	st.Referrers["a/b|2026-02-21"] = true
	st.Paths["a/b|2026-02-21"] = true
	if err := store.save(st); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := store.reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	loaded, err := store.load()
	if err != nil {
		t.Fatalf("load after reset: %v", err)
	}
	if len(loaded.Traffic) != 0 || len(loaded.Referrers) != 0 || len(loaded.Paths) != 0 {
		t.Errorf("expected empty state after reset, got %+v", loaded)
	}
}

// --- importState / copyState ---

func TestImportState_RoundTrip(t *testing.T) {
	// Write a legacy JSON state file.
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte(`{
		"traffic":   {"owner/repo|2026-02-15": {"views": 42, "clones": 8}},
		"referrers": {"owner/repo|2026-02-15": true},
		"paths":     {"owner/repo|2026-02-15": true}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newTestSQLiteStore(t)
	if err := importState(path, store); err != nil {
		t.Fatalf("importState: %v", err)
	}

	loaded, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tc := loaded.Traffic["owner/repo|2026-02-15"]
	if tc.Views != 42 || tc.Clones != 8 {
		t.Errorf("traffic = %+v, want {Views:42 Clones:8}", tc)
	}
	if !loaded.Referrers["owner/repo|2026-02-15"] {
		t.Error("expected referrer key")
	}
	if !loaded.Paths["owner/repo|2026-02-15"] {
		t.Error("expected path key")
	}
}

func TestCopyState_BetweenStores(t *testing.T) {
	// copyState is what the SQLite->Postgres migration relies on. Exercise it
	// store-to-store (SQLite both ends) so the logic is covered without a
	// live Postgres.
	src := newTestSQLiteStore(t)
	st := newPushState()
	st.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 7, Clones: 3}
	st.Referrers["a/b|2026-02-21"] = true
	st.Paths["a/b|2026-02-21"] = true
	if err := src.save(st); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	dst := newTestSQLiteStore(t)
	if err := copyState(src, dst); err != nil {
		t.Fatalf("copyState: %v", err)
	}

	loaded, err := dst.load()
	if err != nil {
		t.Fatalf("load destination: %v", err)
	}
	tc := loaded.Traffic["a/b|2026-02-21"]
	if tc.Views != 7 || tc.Clones != 3 {
		t.Errorf("copied traffic = %+v, want {Views:7 Clones:3}", tc)
	}
	if !loaded.Referrers["a/b|2026-02-21"] || !loaded.Paths["a/b|2026-02-21"] {
		t.Error("expected referrer and path snapshots to be copied")
	}
}

// --- pusher HTTP behavior ---

func TestPusher_SendEvent_RequestShape(t *testing.T) {
	var received umamiEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/send" {
			t.Errorf("path = %q, want /api/send", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != userAgent {
			t.Errorf("User-Agent = %q, want %q", ua, userAgent)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL}
	if err := p.pushAll([]umamiEvent{{Type: "event", Payload: eventPayload{Name: "github_clone"}}}); err != nil {
		t.Fatalf("pushAll: %v", err)
	}
	if received.Payload.Name != "github_clone" {
		t.Errorf("received Name = %q, want github_clone", received.Payload.Name)
	}
}

func TestPusher_SendsOneRequestPerEvent(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL}
	events := make([]umamiEvent, 7)
	for i := range events {
		events[i] = umamiEvent{Type: "event"}
	}
	if err := p.pushAll(events); err != nil {
		t.Fatalf("pushAll: %v", err)
	}
	if callCount != 7 {
		t.Errorf("HTTP calls = %d, want 7 (one per event)", callCount)
	}
}

func TestPusher_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL}
	if err := p.pushAll([]umamiEvent{{Type: "event"}}); err == nil {
		t.Error("expected error for HTTP 500, got nil")
	}
}
