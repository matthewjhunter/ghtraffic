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

// --- trafficEvent ---

func TestTrafficEvent_Shape(t *testing.T) {
	r := Record{
		Repo:   "owner/repo",
		Date:   "2026-02-15",
		Views:  DayCounts{Count: 42, Uniques: 15},
		Clones: DayCounts{Count: 8, Uniques: 5},
	}
	e, err := trafficEvent(r, "website-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if e.Type != "event" {
		t.Errorf("Type = %q, want %q", e.Type, "event")
	}
	if e.Payload.Website != "website-uuid" {
		t.Errorf("Website = %q, want %q", e.Payload.Website, "website-uuid")
	}
	if e.Payload.Hostname != "github.com" {
		t.Errorf("Hostname = %q, want %q", e.Payload.Hostname, "github.com")
	}
	if e.Payload.URL != "/owner/repo" {
		t.Errorf("URL = %q, want /owner/repo", e.Payload.URL)
	}
	if e.Payload.Name != "github_traffic" {
		t.Errorf("Name = %q, want github_traffic", e.Payload.Name)
	}
}

func TestTrafficEvent_Timestamp(t *testing.T) {
	r := Record{Repo: "a/b", Date: "2026-02-15"}
	e, err := trafficEvent(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected, _ := time.Parse("2006-01-02", "2026-02-15")
	if e.Payload.Timestamp != expected.UTC().Unix() {
		t.Errorf("Timestamp = %d, want %d (UTC midnight for 2026-02-15)",
			e.Payload.Timestamp, expected.UTC().Unix())
	}
}

func TestTrafficEvent_DataFields(t *testing.T) {
	r := Record{
		Repo:   "owner/repo",
		Date:   "2026-02-15",
		Views:  DayCounts{Count: 42, Uniques: 15},
		Clones: DayCounts{Count: 8, Uniques: 5},
	}
	e, err := trafficEvent(r, "uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := []struct {
		key  string
		want int
	}{
		{"views", 42},
		{"unique_views", 15},
		{"clones", 8},
		{"unique_clones", 5},
	}
	for _, c := range cases {
		v, ok := e.Payload.Data[c.key].(int)
		if !ok || v != c.want {
			t.Errorf("data[%q] = %v, want %d", c.key, e.Payload.Data[c.key], c.want)
		}
	}
	if v, ok := e.Payload.Data["repo"].(string); !ok || v != "owner/repo" {
		t.Errorf("data[repo] = %v, want owner/repo", e.Payload.Data["repo"])
	}
}

func TestTrafficEvent_BadDate(t *testing.T) {
	_, err := trafficEvent(Record{Repo: "a/b", Date: "not-a-date"}, "uuid")
	if err == nil {
		t.Error("expected error for invalid date, got nil")
	}
}

// --- referrerEvents ---

func TestReferrerEvents(t *testing.T) {
	refs := []Referrer{
		{Name: "google.com", Count: 10, Uniques: 8},
		{Name: "github.com", Count: 5, Uniques: 4},
	}
	ts := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)
	events := referrerEvents("owner/repo", refs, ts, "website-uuid")

	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
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
	if v, _ := events[0].Payload.Data["referrer"].(string); v != "google.com" {
		t.Errorf("first referrer = %q, want google.com", v)
	}
	if v, _ := events[1].Payload.Data["referrer"].(string); v != "github.com" {
		t.Errorf("second referrer = %q, want github.com", v)
	}
}

func TestReferrerEvents_Empty(t *testing.T) {
	events := referrerEvents("a/b", nil, time.Now(), "uuid")
	if len(events) != 0 {
		t.Errorf("expected empty events for nil referrers, got %d", len(events))
	}
}

// --- pathEvents ---

func TestPathEvents(t *testing.T) {
	paths := []Path{
		{Path: "/owner/repo/blob/main/README.md", Title: "README", Count: 20, Uniques: 12},
	}
	ts := time.Date(2026, 2, 21, 0, 0, 0, 0, time.UTC)
	events := pathEvents("owner/repo", paths, ts, "uuid")

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Payload.Name != "github_path" {
		t.Errorf("Name = %q, want github_path", events[0].Payload.Name)
	}
	if v, _ := events[0].Payload.Data["path"].(string); v != "/owner/repo/blob/main/README.md" {
		t.Errorf("path = %q, want /owner/repo/blob/main/README.md", v)
	}
	if v, _ := events[0].Payload.Data["title"].(string); v != "README" {
		t.Errorf("title = %q, want README", v)
	}
	if v, _ := events[0].Payload.Data["count"].(int); v != 20 {
		t.Errorf("count = %v, want 20", v)
	}
}

// --- buildEvents ---

func TestBuildEvents_SkipsPushed(t *testing.T) {
	records := []Record{
		{Repo: "a/b", Date: "2026-02-20", CollectedAt: "2026-02-21T00:00:00Z"},
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z"},
	}
	pushed := map[string]bool{
		"t|a/b|2026-02-20": true,
	}

	events, newKeys := buildEvents(records, "uuid", pushed)

	if _, ok := newKeys["t|a/b|2026-02-21"]; !ok {
		t.Error("expected new key t|a/b|2026-02-21")
	}
	if _, ok := newKeys["t|a/b|2026-02-20"]; ok {
		t.Error("already-pushed key t|a/b|2026-02-20 must not be in newKeys")
	}

	var trafficCount int
	for _, e := range events {
		if e.Payload.Name == "github_traffic" {
			trafficCount++
		}
	}
	if trafficCount != 1 {
		t.Errorf("trafficCount = %d, want 1", trafficCount)
	}
}

func TestBuildEvents_ReferrersFromLatestRecord(t *testing.T) {
	older := Record{
		Repo:        "a/b",
		Date:        "2026-02-19",
		CollectedAt: "2026-02-20T00:00:00Z",
		Referrers:   []Referrer{{Name: "old.com", Count: 1, Uniques: 1}},
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
	if len(refSources) != 1 || refSources[0] != "new.com" {
		t.Errorf("referrer sources = %v, want [new.com]", refSources)
	}
}

func TestBuildEvents_SkipsPushedReferrers(t *testing.T) {
	r := Record{
		Repo:        "a/b",
		Date:        "2026-02-21",
		CollectedAt: "2026-02-21T00:00:00Z",
		Referrers:   []Referrer{{Name: "google.com", Count: 5, Uniques: 3}},
	}
	pushed := map[string]bool{
		"r|a/b|2026-02-21": true,
	}
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
			t.Errorf("unexpected event %q when referrers/paths are empty", e.Payload.Name)
		}
	}
}

func TestBuildEvents_MultipleRepos(t *testing.T) {
	records := []Record{
		{Repo: "a/b", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 10, Uniques: 5}},
		{Repo: "c/d", Date: "2026-02-21", CollectedAt: "2026-02-21T00:00:00Z", Views: DayCounts{Count: 20, Uniques: 8}},
	}
	events, newKeys := buildEvents(records, "uuid", map[string]bool{})

	if _, ok := newKeys["t|a/b|2026-02-21"]; !ok {
		t.Error("expected key t|a/b|2026-02-21")
	}
	if _, ok := newKeys["t|c/d|2026-02-21"]; !ok {
		t.Error("expected key t|c/d|2026-02-21")
	}

	var trafficCount int
	for _, e := range events {
		if e.Payload.Name == "github_traffic" {
			trafficCount++
		}
	}
	if trafficCount != 2 {
		t.Errorf("trafficCount = %d, want 2", trafficCount)
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
			t.Errorf("expected key %q to be present after reload", k)
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
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
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
		{Type: "event", Payload: eventPayload{Website: "uuid", Hostname: "github.com", URL: "/a/b", Name: "github_traffic", Timestamp: 1708387200}},
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
			Name:      "github_traffic",
			Timestamp: 1708387200,
			Data: map[string]any{
				"repo":          "owner/repo",
				"views":         42,
				"unique_views":  15,
				"clones":        8,
				"unique_clones": 5,
			},
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
	if payload["website"] != "test-uuid" {
		t.Errorf("payload.website = %v, want test-uuid", payload["website"])
	}
	if payload["timestamp"] != float64(1708387200) {
		t.Errorf("payload.timestamp = %v, want 1708387200", payload["timestamp"])
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatal("payload.data is not an object")
	}
	if data["views"] != float64(42) {
		t.Errorf("data.views = %v, want 42", data["views"])
	}
}
