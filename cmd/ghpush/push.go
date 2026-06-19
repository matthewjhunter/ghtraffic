package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// Umami's bot-detection blocks non-browser user-agents and silently returns
// {"beep":"boop"} instead of recording the event. Use a browser UA so our
// historical import events are actually stored.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"

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

// umamiEvent is a single event in Umami's send API format.
type umamiEvent struct {
	Type    string       `json:"type"`
	Payload eventPayload `json:"payload"`
}

type eventPayload struct {
	Website   string         `json:"website"`
	Hostname  string         `json:"hostname"`
	Screen    string         `json:"screen,omitempty"`
	Language  string         `json:"language,omitempty"`
	Title     string         `json:"title,omitempty"`
	URL       string         `json:"url"`
	Referrer  string         `json:"referrer,omitempty"`
	Name      string         `json:"name,omitempty"`
	Timestamp int64          `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"`
}

// buildEvents converts ghtraffic records into Umami events, emitting only the
// delta since the last push for each repo+date. Returns the events to send and
// an updated state to persist after a successful send.
func buildEvents(records []Record, websiteID string, st pushState, now time.Time) ([]umamiEvent, pushState) {
	today := now.UTC().Format("2006-01-02")

	next := newPushState()
	maps.Copy(next.Traffic, st.Traffic)
	maps.Copy(next.Referrers, st.Referrers)
	maps.Copy(next.Paths, st.Paths)

	latestByRepo := make(map[string]Record)
	for _, r := range records {
		if prev, ok := latestByRepo[r.Repo]; !ok || r.Date > prev.Date {
			latestByRepo[r.Repo] = r
		}
	}

	var events []umamiEvent

	for _, r := range records {
		key := r.Repo + "|" + r.Date
		prev := next.Traffic[key]

		viewDelta := r.Views.Count - prev.Views
		// Use Uniques for clones: total clone count is dominated by CI
		// (GitHub Actions, Dependabot) which inflates numbers ~10x. Unique
		// cloners is a better proxy for actual human interest.
		cloneDelta := r.Clones.Uniques - prev.Clones
		if viewDelta <= 0 && cloneDelta <= 0 {
			continue
		}

		ts := dayTimestamp(r.Date, r.CollectedAt, today)

		if viewDelta > 0 {
			// No Name → Umami v2 treats this as a pageview (shows in main chart).
			key := r.Repo + "|" + r.Date
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				Screen: sessionScreen(key), Language: "en-US",
				URL: "/" + r.Repo, Timestamp: ts,
			}}
			events = append(events, repeatEvent(e, viewDelta)...)
		}
		if cloneDelta > 0 {
			// Clones as pageviews under /clone/ prefix with a distinct session
			// key so they appear as separate visitors from view traffic.
			key := "clone/" + r.Repo + "|" + r.Date
			e := umamiEvent{Type: "event", Payload: eventPayload{
				Website: websiteID, Hostname: "github.com",
				Screen: sessionScreen(key), Language: "en-US",
				URL: "/clone/" + r.Repo, Timestamp: ts,
			}}
			events = append(events, repeatEvent(e, cloneDelta)...)
		}

		next.Traffic[key] = trafficCounts{
			Views:  max(r.Views.Count, prev.Views),
			Clones: max(r.Clones.Uniques, prev.Clones),
		}
	}

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

// dayTimestamp returns the Umami event timestamp for a record in Unix seconds.
// Umami's /api/send endpoint uses Unix seconds for the timestamp field.
// Historical dates use UTC midnight; today's date uses CollectedAt.
func dayTimestamp(date, collectedAt, today string) int64 {
	if date == today {
		if t, err := time.Parse(time.RFC3339, collectedAt); err == nil {
			return t.UTC().Unix()
		}
	}
	t, _ := time.Parse("2006-01-02", date)
	return t.UTC().Unix()
}

// sessionScreen returns a deterministic screen resolution derived from key.
func sessionScreen(key string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return fmt.Sprintf("1920x%d", 900+h.Sum32()%1000)
}

// referrerEvents builds one pageview per referral hit with the Referrer
// payload field set so Umami populates its built-in referrer breakdown.
func referrerEvents(repo string, refs []Referrer, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	for _, ref := range refs {
		referrerURL := ref.Name
		if !strings.Contains(ref.Name, "://") {
			referrerURL = "https://" + ref.Name
		}
		// Use a different hostname so github.com referrers are not stripped
		// as self-referrals by Umami (which filters referrer_domain==hostname).
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "traffic.github.com",
			Screen: sessionScreen(ref.Name), Language: "en-US",
			URL: "/" + repo, Referrer: referrerURL,
			Timestamp: ts,
		}}
		out = append(out, repeatEvent(e, ref.Count)...)
	}
	return out
}

// pathEvents builds one pageview per path hit using the GitHub path as the URL.
// This populates Umami's top-pages breakdown with the actual repo subpaths.
// No Name → Umami v2 treats events without a name as pageviews.
func pathEvents(repo string, paths []Path, collectedAt time.Time, websiteID string) []umamiEvent {
	ts := collectedAt.UTC().Unix()
	var out []umamiEvent
	pathKey := "path/" + repo
	for _, p := range paths {
		e := umamiEvent{Type: "event", Payload: eventPayload{
			Website: websiteID, Hostname: "github.com",
			Screen: sessionScreen(pathKey), Language: "en-US",
			URL: p.Path, Timestamp: ts,
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

// pusher sends Umami events over HTTP to /api/send one at a time.
type pusher struct {
	httpClient *http.Client
	baseURL    string
}

func (p *pusher) pushAll(events []umamiEvent) error {
	for i, e := range events {
		if err := p.sendEvent(e); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	return nil
}

func (p *pusher) sendEvent(e umamiEvent) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, p.baseURL+"/api/send", bytes.NewReader(body))
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
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
