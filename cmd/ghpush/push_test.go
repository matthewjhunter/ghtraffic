package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- repeatEvent ---

func TestRepeatEvent_ZeroCount(t *testing.T) {
	e := umamiEvent{Type: "event"}
	if got := repeatEvent(e, 0); got != nil {
		t.Errorf("expected nil for count=0, got %v", got)
	}
}

func TestRepeatEvent_NegativeCount(t *testing.T) {
	e := umamiEvent{Type: "event"}
	if got := repeatEvent(e, -1); got != nil {
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

// --- trafficEvents ---

func TestTrafficEvents_Counts(t *testing.T) {
	r := Record{
		Repo:   "owner/repo",
		Date:   "2026-02-15",
		Views:  DayCounts{Count: 7, Uniques: 3},
		Clones: DayCounts{Count: 2, Uniques: 1},
	}
	views, clones, err := trafficEvents(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(views) != 7 {
		t.Errorf("len(views) = %d, want 7", len(views))
	}
	if len(clones) != 2 {
		t.Errorf("len(clones) = %d, want 2", len(clones))
	}
}

func TestTrafficEvents_EventShape(t *testing.T) {
	r := Record{
		Repo:   "owner/repo",
		Date:   "2026-02-15",
		Views:  DayCounts{Count: 1},
		Clones: DayCounts{Count: 1},
	}
	views, clones, err := trafficEvents(r, "website-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v := views[0]
	if v.Type != "event" {
		t.Errorf("Type = %q, want event", v.Type)
	}
	if v.Payload.Website != "website-uuid" {
		t.Errorf("Website = %q, want website-uuid", v.Payload.Website)
	}
	if v.Payload.Hostname != "github.com" {
		t.Errorf("Hostname = %q, want github.com", v.Payload.Hostname)
	}
	if v.Payload.URL != "/owner/repo" {
		t.Errorf("URL = %q, want /owner/repo", v.Payload.URL)
	}
	if v.Payload.Name != "github_view" {
		t.Errorf("view Name = %q, want github_view", v.Payload.Name)
	}
	if clones[0].Payload.Name != "github_clone" {
		t.Errorf("clone Name = %q, want github_clone", clones[0].Payload.Name)
	}
}

func TestTrafficEvents_Timestamp(t *testing.T) {
	r := Record{Repo: "a/b", Date: "2026-02-15", Views: DayCounts{Count: 1}}
	views, _, err := trafficEvents(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected, _ := time.Parse("2006-01-02", "2026-02-15")
	if views[0].Payload.Timestamp != expected.UTC().Unix() {
		t.Errorf("Timestamp = %d, want %d (UTC midnight 2026-02-15)",
			views[0].Payload.Timestamp, expected.UTC().Unix())
	}
}

func TestTrafficEvents_RepoInData(t *testing.T) {
	r := Record{Repo: "owner/repo", Date: "2026-02-15", Views: DayCounts{Count: 1}}
	views, _, err := trafficEvents(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo, _ := views[0].Payload.Data["repo"].(string); repo != "owner/repo" {
		t.Errorf("data[repo] = %q, want owner/repo", repo)
	}
}

func TestTrafficEvents_ZeroCounts(t *testing.T) {
	r := Record{Repo: "a/b", Date: "2026-02-15"}
	views, clones, err := trafficEvents(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("expected 0 view events for zero count, got %d", len(views))
	}
	if len(clones) != 0 {
		t.Errorf("expected 0 clone events for zero count, got %d", len(clones))
	}
}

func TestTrafficEvents_BadDate(t *testing.T) {
	_, _, err := trafficEvents(Record{Repo: "a/b", Date: "not-a-date"}, "uuid")
	if err == nil {
		t.Error("expected error for invalid date, got nil")
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

	if len(events) != 15 { // 10 + 5
		t.Fatalf("len(events) = %d, want 15 (10+5)", len(events))
	}
	for _, e := range events {
		if e.Payload.Name != "github_referrer" {
			t.Errorf("Name = %q, want github_referrer", e.Payload.Name)
		}
		if e.Payload.URL != "/owner/repo" {
			t.Errorf("URL = %q, want /owner/repo", e.Payload.URL)
		}
		if e.Payload.Timestamp != ts.Unix() {
			t.Errorf("Timestamp = %d, want %d", e.Payload.Timestamp, ts.Unix())
		}
	}
	// First 10 events should carry google.com referrer.
	if v, _ := events[0].Payload.Data["referrer"].(string); v != "google.com" {
		t.Errorf("events[0] referrer = %q, want google.com", v)
	}
	// Next 5 should carry github.com.
	if v, _ := events[10].Payload.Data["referrer"].(string); v != "github.com" {
		t.Errorf("events[10] referrer = %q, want github.com", v)
	}
}

func TestReferrerEvents_Empty(t *testing.T) {
	if got := referrerEvents("a/b", nil, time.Now(), "uuid"); len(got) != 0 {
		t.Errorf("expected empty slice for nil referrers, got %d events", len(got))
	}
}

func TestReferrerEvents_ZeroCount(t *testing.T) {
	refs := []Referrer{{Name: "google.com", Count: 0}}
	got := referrerEvents("a/b", refs, time.Now(), "uuid")
	if len(got) != 0 {
		t.Errorf("expected 0 events for referrer with count=0, got %d", len(got))
	}
}

// --- pathEvents ---

func TestPathEvents_Count(t *testing.T) {
	paths := []Path{
		{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 20, Uniques: 12},
		{Path: "/owner/repo/blob/main/main.go", Title: "main.go", Count: 5, Uniques: 3},
	}
	ts := time.Date(2026, 2, 21, 0, 0, 0, 0, time.UTC)
	events := pathEvents("owner/repo", paths, ts, "uuid")

	if len(events) != 25 { // 20 + 5
		t.Fatalf("len(events) = %d, want 25 (20+5)", len(events))
	}
	for _, e := range events {
		if e.Payload.Name != "github_path" {
			t.Errorf("Name = %q, want github_path", e.Payload.Name)
		}
	}
}

func TestPathEvents_URLIsPath(t *testing.T) {
	paths := []Path{
		{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 3},
	}
	events := pathEvents("owner/repo", paths, time.Now(), "uuid")
	if len(events) == 0 {
		t.Fatal("expected events, got none")
	}
	// The path string itself should be the URL so Umami can break down per path.
	if events[0].Payload.URL != "/owner/repo/blob/main/README.md" {
		t.Errorf("URL = %q, want /owner/repo/blob/main/README.md", events[0].Payload.URL)
	}
}

func TestPathEvents_TitleInData(t *testing.T) {
	paths := []Path{{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 1}}
	events := pathEvents("owner/repo", paths, time.Now(), "uuid")
	if title, _ := events[0].Payload.Data["title"].(string); title != "README" {
		t.Errorf("data[title] = %q, want README", title)
	}
}

// --- buildEvents ---

func TestBuildEvents_TrafficCounts(t *testing.T) {
	records := []Record{
		{
			Repo:   "a/b",
			Date:   "2026-02-21",
			Views:  DayCounts{Count: 10, Uniques: 4},
			Clones: DayCounts{Count: 3, Uniques: 2},
		},
	}
	events, _ := buildEvents(records, "uuid", map[string]bool{})

	var viewCount, cloneCount int
	for _, e := range events {
		switch e.Payload.Name {
		case "github_view":
			viewCount++
		case "github_clone":
			cloneCount++
		}
	}
	if viewCount != 10 {
		t.Errorf("viewCount = %d, want 10", viewCount)
	}
	if cloneCount != 3 {
		t.Errorf("cloneCount = %d, want 3", cloneCount)
	}
}

func TestBuildEvents_SkipsPushed(t *testing.T) {
	records := []Record{
		{Repo: "a/b", Date: "2026-02-20", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 5}},
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 3}},
	}
	pushed := map[string]bool{"t|a/b|2026-02-20": true}

	events, newKeys := buildEvents(records, "uuid", pushed)

	if _, ok := newKeys["t|a/b|2026-02-21"]; !ok {
		t.Error("expected new key t|a/b|2026-02-21")
	}
	if _, ok := newKeys["t|a/b|2026-02-20"]; ok {
		t.Error("already-pushed key t|a/b|2026-02-20 must not appear in newKeys")
	}

	var viewCount int
	for _, e := range events {
		if e.Payload.Name == "github_view" {
			viewCount++
		}
	}
	if viewCount != 3 { // only the unpushed date
		t.Errorf("viewCount = %d, want 3 (only 2026-02-21)", viewCount)
	}
}

func TestBuildEvents_ReferrersFromLatestRecord(t *testing.T) {
	older := Record{
		Repo:        "a/b",
		Date:        "2026-02-19",
		CollectedAt: "2026-02-20T00:00:00Z",
		Referrers:   []Referrer{{Name: "old.com", Count: 2, Uniques: 2}},
	}
	newer := Record{
		Repo:        "a/b",
		Date:        "2026-02-21",
		CollectedAt: "2026-02-21T00:00:00Z",
		Referrers:   []Referrer{{Name: "new.com", Count: 5, Uniques: 3}},
	}
	events, _ := buildEvents([]Record{older, newer}, "uuid", map[string]bool{})

	var refSources []string
	for _, e := range events {
		if e.Payload.Name == "github_referrer" {
			if v, ok := e.Payload.Data["referrer"].(string); ok {
				refSources = append(refSources, v)
			}
		}
	}
	// Only new.com (5 events) should appear — from the most-recent record.
	if len(refSources) != 5 {
		t.Errorf("referrer event count = %d, want 5", len(refSources))
	}
	for _, s := range refSources {
		if s != "new.com" {
			t.Errorf("referrer source = %q, want new.com", s)
		}
	}
}

func TestBuildEvents_SkipsPushedReferrers(t *testing.T) {
	r := Record{
		Repo:        "a/b",
		Date:        "2026-02-21",
		CollectedAt: "2026-02-21T00:00:00Z",
		Referrers:   []Referrer{{Name: "google.com", Count: 5, Uniques: 3}},
	}
	pushed := map[string]bool{"r|a/b|2026-02-21": true}
	events, _ := buildEvents([]Record{r}, "uuid", pushed)

	for _, e := range events {
		if e.Payload.Name == "github_referrer" {
			t.Error("expected no referrer events when pushed key exists")
		}
	}
}

func TestBuildEvents_NoReferrersOrPaths(t *testing.T) {
	records := []Record{
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z"},
	}
	events, _ := buildEvents(records, "uuid", map[string]bool{})

	for _, e := range events {
		if e.Payload.Name == "github_referrer" || e.Payload.Name == "github_path" {
			t.Errorf("unexpected %q event when referrers/paths are empty", e.Payload.Name)
		}
	}
}

func TestBuildEvents_MultipleRepos(t *testing.T) {
	records := []Record{
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 4}},
		{Repo: "c/d", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 6}},
	}
	events, newKeys := buildEvents(records, "uuid", map[string]bool{})

	for _, k := range []string{"t|a/b|2026-02-21", "t|c/d|2026-02-21"} {
		if !newKeys[k] {
			t.Errorf("expected new key %q", k)
		}
	}

	var viewCount int
	for _, e := range events {
		if e.Payload.Name == "github_view" {
			viewCount++
		}
	}
	if viewCount != 10 { // 4 + 6
		t.Errorf("viewCount = %d, want 10", viewCount)
	}
}

// --- loadPushed / savePushed ---

func TestLoadPushed_NonExistentFile(t *testing.T) {
	pushed, err := loadPushed(filepath.Join(t.TempDir(), "pushed.txt"))
	if err != nil {
		t.Fatalf("loadPushed: %v", err)
	}
	if len(pushed) != 0 {
		t.Errorf("expected empty map for missing file, got %v", pushed)
	}
}

func TestLoadPushed_EmptyPath(t *testing.T) {
	pushed, err := loadPushed("")
	if err != nil {
		t.Fatalf("loadPushed empty path: %v", err)
	}
	if len(pushed) != 0 {
		t.Error("expected empty map for empty path")
	}
}

func TestSaveAndLoadPushed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pushed.txt")
	original := map[string]bool{
		"t|owner/repo|2026-02-21": true,
		"r|owner/repo|2026-02-21": true,
		"p|owner/repo|2026-02-21": true,
	}

	if err := savePushed(path, original); err != nil {
		t.Fatalf("savePushed: %v", err)
	}
	loaded, err := loadPushed(path)
	if err != nil {
		t.Fatalf("loadPushed: %v", err)
	}
	for k := range original {
		if !loaded[k] {
			t.Errorf("expected key %q after reload", k)
		}
	}
	if len(loaded) != len(original) {
		t.Errorf("loaded %d keys, want %d", len(loaded), len(original))
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
	events := []umamiEvent{
		{Type: "event", Payload: eventPayload{Website: "uuid", Hostname: "github.com", URL: "/a/b", Name: "github_view", Timestamp: 1708387200}},
	}
	if err := p.pushAll(events); err != nil {
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
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Errorf("decode body: %v", err)
		}
		batchSizes = append(batchSizes, len(events))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &pusher{httpClient: srv.Client(), baseURL: srv.URL, batchSize: 3}
	events := make([]umamiEvent, 7)
	for i := range events {
		events[i] = umamiEvent{Type: "event", Payload: eventPayload{Website: "uuid"}}
	}
	if err := p.pushAll(events); err != nil {
		t.Fatalf("pushAll: %v", err)
	}
	// 7 events at batch size 3 → batches of 3, 3, 1.
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

// --- JSON wire format ---

func TestEventPayloadJSON_RoundTrip(t *testing.T) {
	e := umamiEvent{
		Type: "event",
		Payload: eventPayload{
			Website:   "test-uuid",
			Hostname:  "github.com",
			URL:       "/owner/repo",
			Name:      "github_view",
			Timestamp: 1708387200,
			Data:      map[string]any{"repo": "owner/repo"},
		},
	}

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "event" {
		t.Errorf("type = %v, want event", m["type"])
	}
	payload, ok := m["payload"].(map[string]any)
	if !ok {
		t.Fatal("payload is not an object")
	}
	if payload["timestamp"] != float64(1708387200) {
		t.Errorf("payload.timestamp = %v, want 1708387200", payload["timestamp"])
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatal("payload.data is not an object")
	}
	if data["repo"] != "owner/repo" {
		t.Errorf("data.repo = %v, want owner/repo", data["repo"])
	}
}
