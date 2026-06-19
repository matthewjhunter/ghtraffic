# ghtraffic

Collects GitHub traffic data (views, clones, referrers, popular paths) for all
repositories the authenticated user has push access to, including organization
repos. Outputs newline-delimited JSON to stdout, one record per repo per day.
Designed to run hourly via cron.

## Tools

### ghtraffic

Fetches traffic data from the GitHub API and writes NDJSON records to stdout.

```
ghtraffic [-owner OWNER] [-seen FILE]
```

| Flag | Description |
|------|-------------|
| `-owner` | Filter repos to this owner/org (optional) |
| `-seen FILE` | Existing JSONL file for deduplication; today's records are always re-fetched |

Authentication uses `GITHUB_TOKEN`, falling back to `gh auth token`.

### ghpush

Reads ghtraffic NDJSON from stdin and pushes the records to an
[Umami](https://umami.is) instance as pageview events using the `/api/send`
endpoint with historical timestamps.

Requires Umami v2.17 or later.

```
ghpush [-pushed FILE | -pg DSN] [-dry-run] [-init] [-import-json FILE] [-migrate-sqlite FILE]
```

| Flag | Description |
|------|-------------|
| `-pushed FILE` | SQLite state file; tracks what has been pushed to avoid re-sending on re-run |
| `-pg DSN` | Postgres DSN for the state store (alternative to `-pushed`; or set `GHPUSH_DATABASE_URL`) |
| `-dry-run` | Print events as JSON to stdout without sending |
| `-init` | Bootstrap from scratch: ignore push state and push all historical data |
| `-import-json FILE` | Import a legacy JSON state file into the state store and exit |
| `-migrate-sqlite FILE` | Copy an existing SQLite state file into the Postgres store (`-pg`) and exit |
| `-url URL` | Umami base URL (overrides `UMAMI_URL`) |
| `-website UUID` | Umami website UUID (overrides `UMAMI_WEBSITE_ID`) |

**Environment variables:** `UMAMI_URL`, `UMAMI_WEBSITE_ID`, `GHPUSH_DATABASE_URL`

## Usage

```sh
# Collect traffic (skip already-seen repo+date pairs, always re-fetch today)
ghtraffic -seen ~/.local/share/ghtraffic/traffic.jsonl \
  >> ~/.local/share/ghtraffic/traffic.jsonl

# Push deltas to Umami
ghpush -pushed ~/.local/share/ghtraffic/pushed.db \
  < ~/.local/share/ghtraffic/traffic.jsonl
```

### Crontab example

```cron
0  * * * * GITHUB_TOKEN=... ghtraffic -seen ~/traffic.jsonl >> ~/traffic.jsonl
5  * * * * UMAMI_URL=https://umami.example.com UMAMI_WEBSITE_ID=... ghpush -pushed ~/pushed.db < ~/traffic.jsonl
```

## Container deployment

For a long-running deployment, the `scheduler` binary runs one collect+push cycle
on start and then every `INTERVAL_SECONDS`, execing `ghtraffic` (appending to the
data file) then `ghpush`. It is the entrypoint of the published image
`ghcr.io/matthewjhunter/ghtraffic`, built `FROM gcr.io/distroless/static` (no shell,
runs as non-root). Posting to Umami over an internal network address avoids any
reverse-proxy IP filtering that would otherwise drop server-originated events.

| Variable | Default | Description |
|----------|---------|-------------|
| `INTERVAL_SECONDS` | `3600` | Cycle period |
| `GHTRAFFIC_OWNER` | (all) | Restrict collection to one owner/org |
| `DATA_FILE` | `/data/traffic.jsonl` | NDJSON history file (mount a volume here) |
| `BIN_DIR` | `/` | Directory holding the `ghtraffic` and `ghpush` binaries |

The container also reads the binaries' own env: `GITHUB_TOKEN`, `UMAMI_URL`,
`UMAMI_WEBSITE_ID`, and `GHPUSH_DATABASE_URL` (Postgres push-state).

## Umami dashboard notes

**Event mapping:**

| GitHub metric | Umami representation |
|---------------|----------------------|
| Page views | Pageviews to `/<owner>/<repo>` |
| Clones | Pageviews to `/clone/<owner>/<repo>` |
| Referrers | Pageviews with Referrer field set |
| Popular paths | Pageviews to the actual GitHub subpath |

**Unique visitors:** Umami deduplicates visitors by IP address. Since all
events are pushed from a single server, Umami shows a small fixed visitor
count regardless of actual traffic volume. **Ignore the visitor metric.**
Use the **pageview count** for views and the **Pages breakdown** filtered
to `/clone/` for clones — those counts are exact, derived directly from
the GitHub traffic API.
