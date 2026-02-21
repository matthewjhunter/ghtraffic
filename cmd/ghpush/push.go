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

	// One github_traffic event per record.
	for _, r := range records {
		key := "t|" + r.Repo + "|" + r.Date
		if pushed[key] {
			continue
		}
		e, err := trafficEvent(r, websiteID)
		if err != nil {
			continue // malformed date; skip
		}
		events = append(events, e)
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

// trafficEvent builds a github_traffic Umami event from a Record.
func trafficEvent(r Record, websiteID string) (umamiEvent, error) {
	t, err := time.Parse("2006-01-02", r.Date)
	if err != nil {
		return umamiEvent{}, fmt.Errorf("parse date %q: %w", r.Date, err)
	}
	return umamiEvent{
		Type: "event",
		Payload: eventPayload{
			Website:   websiteID,
			Hostname:  "github.com",
			URL:       "/" + r.Repo,
			Name:      "github_traffic",
			Timestamp: t.UTC().Unix(),
			Data: map[string]any{
				"repo":          r.Repo,
				"views":         r.Views.Count,
				"unique_views":  r.Views.Uniques,
				"clones":        r.Clones.Count,
				"unique_clones": r.Clones.Uniques,
			},
		},
	}, nil
}

// referrerEvents builds github_referrer events from a repo's referrer snapshot.
func referrerEvents(repo string, refs []Referrer, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	out := make([]umamiEvent, 0, len(refs))
	for _, ref := range refs {
		out = append(out, umamiEvent{
			Type: "event",
			Payload: eventPayload{
				Website:   websiteID,
				Hostname:  "github.com",
				URL:       "/" + repo,
				Name:      "github_referrer",
				Timestamp: ts,
				Data: map[string]any{
					"repo":     repo,
					"referrer": ref.Name,
					"count":    ref.Count,
					"uniques":  ref.Uniques,
				},
			},
		})
	}
	return out
}

// pathEvents builds github_path events from a repo's popular-paths snapshot.
func pathEvents(repo string, paths []Path, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	out := make([]umamiEvent, 0, len(paths))
	for _, p := range paths {
		out = append(out, umamiEvent{
			Type: "event",
			Payload: eventPayload{
				Website:   websiteID,
				Hostname:  "github.com",
				URL:       "/" + repo,
				Name:      "github_path",
				Timestamp: ts,
				Data: map[string]any{
					"repo":    repo,
					"path":    p.Path,
					"title":   p.Title,
					"count":   p.Count,
					"uniques": p.Uniques,
				},
			},
		})
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
