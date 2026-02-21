package main

import (
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

// pushState tracks what has already been sent to Umami.
type pushState struct {
	// Traffic maps "repo|date" to the cumulative counts already pushed.
	// Deltas are computed against these values on each run.
	Traffic map[string]trafficCounts `json:"traffic"`
	// Referrers and Paths map "repo|collected-date" to a sent flag.
	// Snapshots are sent once per collection date.
	Referrers map[string]bool `json:"referrers"`
	Paths     map[string]bool `json:"paths"`
}

type trafficCounts struct {
	Views  int `json:"views"`
	Clones int `json:"clones"`
}

func newPushState() pushState {
	return pushState{
		Traffic:   make(map[string]trafficCounts),
		Referrers: make(map[string]bool),
		Paths:     make(map[string]bool),
	}
}

// buildEvents converts ghtraffic records into Umami events, emitting only the
// delta since the last push for each repo+date. Returns the events to send and
// an updated state to persist after a successful send.
//
// Traffic events are timestamped to UTC midnight for historical dates so they
// land on the correct day in Umami. For today's date they are timestamped to
// CollectedAt, giving hourly granularity when ghtraffic runs every hour.
//
// Referrer and path snapshots are emitted once per repo per collection date.
func buildEvents(records []Record, websiteID string, st pushState, now time.Time) ([]umamiEvent, pushState) {
	today := now.UTC().Format("2006-01-02")

	// Deep-copy state so the input is not mutated.
	next := newPushState()
	for k, v := range st.Traffic {
		next.Traffic[k] = v
	}
	for k, v := range st.Referrers {
		next.Referrers[k] = v
	}
	for k, v := range st.Paths {
		next.Paths[k] = v
	}

	// Most-recent record per repo for referrer/path snapshot events.
	latestByRepo := make(map[string]Record)
	for _, r := range records {
		if prev, ok := latestByRepo[r.Repo]; !ok || r.Date > prev.Date {
			latestByRepo[r.Repo] = r
		}
	}

	var events []umamiEvent

	// Traffic events — one event per new hit since last push.
	for _, r := range records {
		key := r.Repo + "|" + r.Date
		prev := next.Traffic[key]

		viewDelta := r.Views.Count - prev.Views
		cloneDelta := r.Clones.Count - prev.Clones
		if viewDelta <= 0 && cloneDelta <= 0 {
			continue
		}

		ts := dayTimestamp(r.Date, r.CollectedAt, today)
		data := map[string]any{"repo": r.Repo}

		if viewDelta > 0 {
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				URL: "/" + r.Repo, Name: "github_view",
				Timestamp: ts, Data: data,
			}}
			events = append(events, repeatEvent(e, viewDelta)...)
		}
		if cloneDelta > 0 {
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				URL: "/" + r.Repo, Name: "github_clone",
				Timestamp: ts, Data: data,
			}}
			events = append(events, repeatEvent(e, cloneDelta)...)
		}

		next.Traffic[key] = trafficCounts{
			Views:  max(r.Views.Count, prev.Views),
			Clones: max(r.Clones.Count, prev.Clones),
		}
	}

	// Referrer and path snapshot events — once per repo per collection date.
	for repo, r := range latestByRepo {
		collectedAt, err := time.Parse(time.RFC3339, r.CollectedAt)
		if err != nil {
			collectedAt = now
		}
		collectedDate := collectedAt.UTC().Format("2006-01-02")

		refKey := repo + "|" + collectedDate
		if !next.Referrers[refKey] && len(r.Referrers) > 0 {
			events = append(events, referrerEvents(repo, r.Referrers, collectedAt, websiteID)...)
			next.Referrers[refKey] = true
		}

		pathKey := repo + "|" + collectedDate
		if !next.Paths[pathKey] && len(r.Paths) > 0 {
			events = append(events, pathEvents(repo, r.Paths, collectedAt, websiteID)...)
			next.Paths[pathKey] = true
		}
	}

	return events, next
}

// dayTimestamp returns the Umami event timestamp for a record.
// Historical dates use UTC midnight so events land on the correct date.
// Today's date uses CollectedAt to provide hourly granularity.
func dayTimestamp(date, collectedAt, today string) int64 {
	if date == today {
		if t, err := time.Parse(time.RFC3339, collectedAt); err == nil {
			return t.UTC().Unix()
		}
	}
	t, _ := time.Parse("2006-01-02", date)
	return t.UTC().Unix()
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

// loadState reads the push state from a JSON file. Returns an empty state if
// path is empty, the file does not exist, or the file cannot be parsed.
func loadState(path string) (pushState, error) {
	if path == "" {
		return newPushState(), nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return newPushState(), nil
	}
	if err != nil {
		return newPushState(), err
	}
	defer f.Close()

	var st pushState
	if err := json.NewDecoder(f).Decode(&st); err != nil {
		// Treat a malformed file as empty rather than failing hard.
		return newPushState(), nil
	}
	// Ensure maps are non-nil after decode.
	if st.Traffic == nil {
		st.Traffic = make(map[string]trafficCounts)
	}
	if st.Referrers == nil {
		st.Referrers = make(map[string]bool)
	}
	if st.Paths == nil {
		st.Paths = make(map[string]bool)
	}
	return st, nil
}

// saveState writes the push state to a JSON file.
func saveState(path string, st pushState) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}
