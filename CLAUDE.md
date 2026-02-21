# ghtraffic

CLI tool that collects GitHub traffic data (views, clones, referrers, popular paths) for all repositories the authenticated user has push access to, including organization repos. Outputs newline-delimited JSON to stdout, one record per repo per day. Designed to run daily via cron.

Authentication uses `GITHUB_TOKEN` env var, falling back to `gh auth token`.

## Binaries

- **ghtraffic** — collects traffic data from GitHub API, writes NDJSON to stdout
- **ghpush** (`cmd/ghpush/`) — reads ghtraffic NDJSON from stdin, pushes to Umami via `/api/send`; requires Umami v2.17+

Typical pipeline:

```bash
ghtraffic -seen traffic.jsonl >> traffic.jsonl
ghpush -pushed pushed.db < traffic.jsonl
```

## Build

```bash
go build -o ghtraffic .
go build -o ghpush ./cmd/ghpush
# or: task build
```

## ghpush Flags

| Flag | Env var | Description |
|---|---|---|
| `-url` | `UMAMI_URL` | Umami instance base URL |
| `-website` | `UMAMI_WEBSITE_ID` | Website UUID from Umami settings |
| `-pushed` | — | SQLite state file (`.db`) tracking pushed counts; prevents re-pushing on re-run |
| `-dry-run` | — | Print events as JSON to stdout without sending |
| `-init` | — | Bootstrap from scratch: ignore stored state, push all historical data, reset state baseline |
| `-import-json` | — | Migrate a legacy JSON state file into the SQLite DB and exit (requires `-pushed`) |

### Workflows

**Normal hourly run** (delta events only):
```bash
ghtraffic -seen traffic.jsonl >> traffic.jsonl
ghpush -pushed pushed.db < traffic.jsonl
```

**Bootstrap a fresh Umami site** from all stored history:
```bash
ghpush -pushed pushed.db -init < traffic.jsonl
```

**Migrate from the legacy JSON state file** to SQLite:
```bash
ghpush -pushed pushed.db -import-json pushed.json
```

**Preview events without sending:**
```bash
ghpush -website uuid -dry-run < traffic.jsonl
ghpush -pushed pushed.db -init -dry-run < traffic.jsonl
```

## Test

```bash
go test -race -count=1 ./...
```

## Lint

```bash
golangci-lint run ./...
```

## Vulnerability Check

```bash
govulncheck ./...
```

## All CI Checks

```bash
task check
```
