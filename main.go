// ghtraffic collects GitHub traffic data (views, clones, referrers, popular
// paths) for all repositories the authenticated user has push access to,
// including organization repos. Outputs newline-delimited JSON to stdout,
// one record per repo per day. Designed to run daily via cron.
//
// Authentication: uses GITHUB_TOKEN env var, falling back to `gh auth token`.
//
// Usage:
//
//	ghtraffic >> traffic.jsonl
//	ghtraffic -owner matthewjhunter >> traffic.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// Record is one day's traffic data for a single repository.
type Record struct {
	CollectedAt string     `json:"collected_at"`
	Repo        string     `json:"repo"`
	Date        string     `json:"date"`
	Views       DayCounts  `json:"views"`
	Clones      DayCounts  `json:"clones"`
	Referrers   []Referrer `json:"referrers,omitempty"`
	Paths       []Path     `json:"paths,omitempty"`
}

type DayCounts struct {
	Count   int `json:"count"`
	Uniques int `json:"uniques"`
}

type Referrer struct {
	Name    string `json:"referrer"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

type Path struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Count   int    `json:"count"`
	Uniques int    `json:"uniques"`
}

// GitHub API response types.

type repo struct {
	FullName    string      `json:"full_name"`
	Permissions permissions `json:"permissions"`
}

type permissions struct {
	Push bool `json:"push"`
}

type trafficViews struct {
	Views []dailyCount `json:"views"`
}

type trafficClones struct {
	Clones []dailyCount `json:"clones"`
}

type dailyCount struct {
	Timestamp time.Time `json:"timestamp"`
	Count     int       `json:"count"`
	Uniques   int       `json:"uniques"`
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	owner := flag.String("owner", "", "filter repos to this owner/org (optional)")
	flag.Parse()

	token := resolveToken()
	if token == "" {
		log.Fatal("no GitHub token: set GITHUB_TOKEN or install gh CLI")
	}

	client := &apiClient{token: token, http: &http.Client{Timeout: 30 * time.Second}}
	now := time.Now().UTC().Format(time.RFC3339)
	enc := json.NewEncoder(os.Stdout)

	repos, err := client.listPushRepos()
	if err != nil {
		log.Fatalf("list repos: %v", err)
	}

	for _, r := range repos {
		if *owner != "" && !strings.HasPrefix(r, *owner+"/") {
			continue
		}

		views, clones, referrers, paths, err := client.collectTraffic(r)
		if err != nil {
			log.Printf("skip %s: %v", r, err)
			continue
		}

		// Merge views and clones by date into records.
		days := mergeDays(views, clones)
		if len(days) == 0 {
			// Emit one record even with no daily data so we know the repo was checked.
			rec := Record{
				CollectedAt: now,
				Repo:        r,
				Date:        time.Now().UTC().Format("2006-01-02"),
				Referrers:   referrers,
				Paths:       paths,
			}
			enc.Encode(rec)
			continue
		}

		for _, d := range days {
			rec := Record{
				CollectedAt: now,
				Repo:        r,
				Date:        d.date,
				Views:       d.views,
				Clones:      d.clones,
				Referrers:   referrers,
				Paths:       paths,
			}
			enc.Encode(rec)
		}

		log.Printf("collected %s (%d days)", r, len(days))
	}
}

func resolveToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// apiClient wraps authenticated GitHub API calls.
type apiClient struct {
	token string
	http  *http.Client
}

func (c *apiClient) get(path string, target any) error {
	url := apiBase + path
	for {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 403 || resp.StatusCode == 429 {
			// Rate limited -- check Retry-After or back off.
			wait := 60 * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if d, err := time.ParseDuration(ra + "s"); err == nil {
					wait = d
				}
			}
			log.Printf("rate limited, waiting %s", wait)
			time.Sleep(wait)
			continue
		}

		if resp.StatusCode != 200 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
		}

		if err != nil {
			return err
		}
		return json.Unmarshal(body, target)
	}
}

// listPushRepos returns full_name of every repo the user can push to.
func (c *apiClient) listPushRepos() ([]string, error) {
	var all []string
	page := 1
	for {
		var repos []repo
		path := fmt.Sprintf("/user/repos?per_page=100&page=%d&type=all", page)
		if err := c.get(path, &repos); err != nil {
			return nil, err
		}
		if len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if r.Permissions.Push {
				all = append(all, r.FullName)
			}
		}
		page++
	}
	return all, nil
}

func (c *apiClient) collectTraffic(repo string) ([]dailyCount, []dailyCount, []Referrer, []Path, error) {
	prefix := "/repos/" + repo + "/traffic"

	var views trafficViews
	if err := c.get(prefix+"/views", &views); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("views: %w", err)
	}

	var clones trafficClones
	if err := c.get(prefix+"/clones", &clones); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("clones: %w", err)
	}

	var referrers []Referrer
	if err := c.get(prefix+"/popular/referrers", &referrers); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("referrers: %w", err)
	}

	var paths []Path
	if err := c.get(prefix+"/popular/paths", &paths); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("paths: %w", err)
	}

	return views.Views, clones.Clones, referrers, paths, nil
}

type dayData struct {
	date   string
	views  DayCounts
	clones DayCounts
}

func mergeDays(views, clones []dailyCount) []dayData {
	byDate := make(map[string]*dayData)

	for _, v := range views {
		d := v.Timestamp.UTC().Format("2006-01-02")
		dd, ok := byDate[d]
		if !ok {
			dd = &dayData{date: d}
			byDate[d] = dd
		}
		dd.views = DayCounts{Count: v.Count, Uniques: v.Uniques}
	}

	for _, c := range clones {
		d := c.Timestamp.UTC().Format("2006-01-02")
		dd, ok := byDate[d]
		if !ok {
			dd = &dayData{date: d}
			byDate[d] = dd
		}
		dd.clones = DayCounts{Count: c.Count, Uniques: c.Uniques}
	}

	// Sort by date.
	days := make([]dayData, 0, len(byDate))
	for _, dd := range byDate {
		days = append(days, *dd)
	}
	for i := range days {
		for j := i + 1; j < len(days); j++ {
			if days[j].date < days[i].date {
				days[i], days[j] = days[j], days[i]
			}
		}
	}

	return days
}
