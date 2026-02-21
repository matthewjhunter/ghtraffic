package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const userAgent = "ghpush/1.0 (+https://github.com/matthewjhunter/ghtraffic)"

// Record mirrors the ghtraffic NDJSON output format.
type Record struct {
	CollectedAt string     `json:"collected_at"`
	Repo        string     `json:"repo"`
	Date        string     `json:"date"`
	Views       DayCounts  `json:"views"`
	Clones      DayCounts  `json:"clones"`
	Referrers   []Referrer `json:"referrers,omitempty"`
	Paths       []Path     `json:"paths,omitempty"`
}

// DayCounts holds total and unique counts for a traffic metric.
type DayCounts struct {
	Count   int `json:"count"`
	Uniques int `json:"uniques"`
}

// Referrer is a traffic source entry.
type Referrer struct {
	Name    string `json:"referrer"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

// Path is a popular-paths entry.
type Path struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

// umamiEvent is a single event in Umami's send/batch API format.
type umamiEvent struct {
	Type    string       `json:"type"`
	Payload eventPayload `json:"payload"`
}

type eventPayload struct {
	Website   string         `json:"website"`
	Hostname  string         `json:"hostname"`
	URL       string         `json:"url"`
	Name      string         `json:"name"`
	Timestamp int64          `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// buildEvents converts ghtraffic records into Umami events, skipping any
// repo+date combination already present in pushed. Returns the events to send
// and the set of new state keys generated (to be merged into pushed after a
// successful send).
//
// Each view, clone, referral, and path hit is emitted as an individual event
// so Umami's native event-count charts reflect actual traffic numbers without
// any custom querying.
func buildEvents(records []Record, websiteID string, pushed map[string]bool) ([]umamiEvent, map[string]bool) {
	newKeys := make(map[string]bool)
	var events []umamiEvent

	// Determine the most-recent record per repo for referrer/path events.
	// Referrers and paths from GitHub are rolling 14-day snapshots, not
	// time-series, so we emit them once from the latest record only.
	latestByRepo := make(map[string]Record)
	for _, r := range records {
		if prev, ok := latestByRepo[r.Repo]; !ok || r.Date > prev.Date {
			latestByRepo[r.Repo] = r
		}
	}

	// github_view and github_clone — one event per hit, per day.
	for _, r := range records {
		key := "t|" + r.Repo + "|" + r.Date
		if pushed[key] {
			continue
		}
		vs, cs, err := trafficEvents(r, websiteID)
		if err != nil {
			continue // malformed date; skip
		}
		events = append(events, vs...)
		events = append(events, cs...)
		newKeys[key] = true
	}

	// github_referrer and github_path events — once per repo.
	for repo, r := range latestByRepo {
		collectedAt, err := time.Parse(time.RFC3339, r.CollectedAt)
		if err != nil {
			collectedAt = time.Now().UTC()
		}
		collectedDate := collectedAt.UTC().Format("2006-01-02")

		refKey := "r|" + repo + "|" + collectedDate
		if !pushed[refKey] && len(r.Referrers) > 0 {
			events = append(events, referrerEvents(repo, r.Referrers, collectedAt, websiteID)...)
			newKeys[refKey] = true
		}

		pathKey := "p|" + repo + "|" + collectedDate
		if !pushed[pathKey] && len(r.Paths) > 0 {
			events = append(events, pathEvents(repo, r.Paths, collectedAt, websiteID)...)
			newKeys[pathKey] = true
		}
	}

	return events, newKeys
}

// trafficEvents returns one github_view event per view hit and one github_clone
// event per clone hit for the given record. Umami's event count charts will
// then display actual traffic numbers natively.
func trafficEvents(r Record, websiteID string) (views, clones []umamiEvent, err error) {
	t, err := time.Parse("2006-01-02", r.Date)
	if err != nil {
		return nil, nil, fmt.Errorf("parse date %q: %w", r.Date, err)
	}
	ts := t.UTC().Unix()
	data := map[string]any{"repo": r.Repo}

	view := umamiEvent{Type: "event", Payload: eventPayload{
		Website: websiteID, Hostname: "github.com",
		URL: "/" + r.Repo, Name: "github_view",
		Timestamp: ts, Data: data,
	}}
	clone := umamiEvent{Type: "event", Payload: eventPayload{
		Website: websiteID, Hostname: "github.com",
		URL: "/" + r.Repo, Name: "github_clone",
		Timestamp: ts, Data: data,
	}}
	return repeatEvent(view, r.Views.Count), repeatEvent(clone, r.Clones.Count), nil
}

// referrerEvents builds one github_referrer event per referral hit from a
// repo's rolling referrer snapshot.
func referrerEvents(repo string, refs []Referrer, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	for _, ref := range refs {
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "github.com",
			URL: "/" + repo, Name: "github_referrer",
			Timestamp: ts, Data: map[string]any{"repo": repo, "referrer": ref.Name},
		}}
		out = append(out, repeatEvent(e, ref.Count)...)
	}
	return out
}

// pathEvents builds one github_path event per hit from a repo's popular-paths
// snapshot. The path string is used as the Umami URL so path-level breakdowns
// work naturally in the Umami UI.
func pathEvents(repo string, paths []Path, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	for _, p := range paths {
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "github.com",
			URL: p.Path, Name: "github_path",
			Timestamp: ts, Data: map[string]any{"repo": repo, "title": p.Title},
		}}
		out = append(out, repeatEvent(e, p.Count)...)
	}
	return out
}

// repeatEvent returns a slice of n copies of e. Returns nil if n <= 0.
func repeatEvent(e umamiEvent, n int) []umamiEvent {
	if n <= 0 {
		return nil
	}
	out := make([]umamiEvent, n)
	for i := range out {
		out[i] = e
	}
	return out
}

// pusher sends batches of Umami events over HTTP.
type pusher struct {
	httpClient *http.Client
	baseURL    string
	batchSize  int
}

func (p *pusher) pushAll(events []umamiEvent) error {
	for i := 0; i < len(events); i += p.batchSize {
		end := min(i+p.batchSize, len(events))
		if err := p.sendBatch(events[i:end]); err != nil {
			return fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
	}
	return nil
}

func (p *pusher) sendBatch(events []umamiEvent) error {
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/api/batch", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// loadPushed reads the pushed-state file and returns the set of already-pushed
// keys. Returns an empty set if path is empty or the file does not exist.
func loadPushed(path string) (map[string]bool, error) {
	pushed := make(map[string]bool)
	if path == "" {
		return pushed, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return pushed, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if k := scanner.Text(); k != "" {
			pushed[k] = true
		}
	}
	return pushed, scanner.Err()
}

// savePushed writes the full pushed-state map as one key per line.
func savePushed(path string, pushed map[string]bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for k := range pushed {
		if _, err := fmt.Fprintln(w, k); err != nil {
			return err
		}
	}
	return w.Flush()
}
