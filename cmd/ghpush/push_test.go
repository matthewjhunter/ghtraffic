package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			Clones:      DayCounts{Count: 3},
		},
	}
	now := time.Date(2026, 2, 21, 9, 0, 0, 0, time.UTC)
	events, newSt := buildEvents(records, "uuid", newPushState(), now)

	var views, clones int
	for _, e := range events {
		switch e.Payload.Name {
		case "github_view":
			views++
		case "github_clone":
			clones++
		}
	}
	if views != 10 {
		t.Errorf("views = %d, want 10 (full count on first run)", views)
	}
	if clones != 3 {
		t.Errorf("clones = %d, want 3 (full count on first run)", clones)
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
			Clones:      DayCounts{Count: 5},
		},
	}
	now := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	events, newSt := buildEvents(records, "uuid", st, now)

	var views, clones int
	for _, e := range events {
		switch e.Payload.Name {
		case "github_view":
			views++
		case "github_clone":
			clones++
		}
	}
	if views != 5 { // 15 - 10
		t.Errorf("views = %d, want 5 (delta)", views)
	}
	if clones != 2 { // 5 - 3
		t.Errorf("clones = %d, want 2 (delta)", clones)
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
			Views:       DayCounts{Count: 10}, // same as state
			Clones:      DayCounts{Count: 3},
		},
	}
	now := time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC)
	events, _ := buildEvents(records, "uuid", st, now)

	for _, e := range events {
		if e.Payload.Name == "github_view" || e.Payload.Name == "github_clone" {
			t.Errorf("unexpected traffic event %q when counts unchanged", e.Payload.Name)
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
		if e.Payload.Name == "github_view" && e.Payload.Timestamp != expected.UTC().Unix() {
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
		if e.Payload.Name == "github_view" && e.Payload.Timestamp != midnight.UTC().Unix() {
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
		if e.Payload.Name == "github_referrer" {
			refCount++
			if v, _ := e.Payload.Data["referrer"].(string); v != "new.com" {
				t.Errorf("referrer = %q, want new.com", v)
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
		if e.Payload.Name == "github_referrer" {
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
	if v, _ := events[0].Payload.Data["referrer"].(string); v != "google.com" {
		t.Errorf("events[0] referrer = %q, want google.com", v)
	}
	if v, _ := events[10].Payload.Data["referrer"].(string); v != "github.com" {
		t.Errorf("events[10] referrer = %q, want github.com", v)
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

// --- loadState / saveState ---

func TestLoadState_NilDB(t *testing.T) {
	st, err := loadState(nil)
	if err != nil {
		t.Fatalf("loadState(nil): %v", err)
	}
	if len(st.Traffic) != 0 || len(st.Referrers) != 0 || len(st.Paths) != 0 {
		t.Errorf("expected empty state for nil db, got %+v", st)
	}
}

func TestSaveAndLoadState(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	original := newPushState()
	original.Traffic["owner/repo|2026-02-21"] = trafficCounts{Views: 42, Clones: 8}
	original.Referrers["owner/repo|2026-02-21"] = true
	original.Paths["owner/repo|2026-02-21"] = true

	if err := saveState(db, original); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	loaded, err := loadState(db)
	if err != nil {
		t.Fatalf("loadState: %v", err)
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
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	st1 := newPushState()
	st1.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 10, Clones: 2}
	if err := saveState(db, st1); err != nil {
		t.Fatalf("saveState 1: %v", err)
	}

	st2 := newPushState()
	st2.Traffic["a/b|2026-02-21"] = trafficCounts{Views: 15, Clones: 5}
	if err := saveState(db, st2); err != nil {
		t.Fatalf("saveState 2: %v", err)
	}

	loaded, err := loadState(db)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	tc := loaded.Traffic["a/b|2026-02-21"]
	if tc.Views != 15 || tc.Clones != 5 {
		t.Errorf("after upsert traffic = %+v, want {Views:15 Clones:5}", tc)
	}
}

func TestSaveState_SnapshotIdempotent(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	st := newPushState()
	st.Referrers["a/b|2026-02-21"] = true
	st.Paths["a/b|2026-02-21"] = true

	// Saving twice must not fail (INSERT OR IGNORE semantics).
	if err := saveState(db, st); err != nil {
		t.Fatalf("saveState 1: %v", err)
	}
	if err := saveState(db, st); err != nil {
		t.Fatalf("saveState 2 (duplicate): %v", err)
	}
}

// --- pusher HTTP behavior ---

func TestPusher_SendBatch_RequestShape(t *testing.T) {
	var received []umamiEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/batch" {
			t.Errorf("path = %q, want /api/batch", r.URL.Path)
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

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL, batchSize: 100}
	if err := p.pushAll([]umamiEvent{{Type: "event", Payload: eventPayload{Name: "github_view"}}}); err != nil {
		t.Fatalf("pushAll: %v", err)
	}
	if len(received) != 1 {
		t.Errorf("received %d events, want 1", len(received))
	}
}

func TestPusher_BatchesBySize(t *testing.T) {
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var events []umamiEvent
		json.NewDecoder(r.Body).Decode(&events)
		batchSizes = append(batchSizes, len(events))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL, batchSize: 3}
	events := make([]umamiEvent, 7)
	for i := range events {
		events[i] = umamiEvent{Type: "event"}
	}
	if err := p.pushAll(events); err != nil {
		t.Fatalf("pushAll: %v", err)
	}
	if len(batchSizes) != 3 {
		t.Fatalf("got %d batches, want 3", len(batchSizes))
	}
	if batchSizes[0] != 3 || batchSizes[1] != 3 || batchSizes[2] != 1 {
		t.Errorf("batchSizes = %v, want [3 3 1]", batchSizes)
	}
}

func TestPusher_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL, batchSize: 100}
	if err := p.pushAll([]umamiEvent{{Type: "event"}}); err == nil {
		t.Error("expected error for HTTP 500, got nil")
	}
}
