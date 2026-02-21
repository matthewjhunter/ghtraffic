# ghtraffic

CLI tool that collects GitHub traffic data (views, clones, referrers, popular paths) for all repositories the authenticated user has push access to, including organization repos. Outputs newline-delimited JSON to stdout, one record per repo per day. Designed to run daily via cron.

Authentication uses `GITHUB_TOKEN` env var, falling back to `gh auth token`.

## Binaries

- **ghtraffic** — collects traffic data from GitHub API, writes NDJSON to stdout
- **ghpush** (`cmd/ghpush/`) — reads ghtraffic NDJSON from stdin, pushes to Umami via `/api/batch`; requires Umami v2.17+

Typical pipeline:

```bash
ghtraffic -seen traffic.jsonl >> traffic.jsonl
ghpush -pushed pushed.txt < traffic.jsonl
```

## Build

```bash
go build -o ghtraffic .
go build -o ghpush ./cmd/ghpush
# or: task build
```

## ghpush Configuration

| Env var | Flag | Description |
|---|---|---|
| `UMAMI_URL` | `-url` | Umami instance base URL |
| `UMAMI_WEBSITE_ID` | `-website` | Website UUID from Umami settings |
| — | `-pushed` | State file to prevent re-pushing on re-run |
| — | `-batch-size` | Events per POST (default 100) |
| — | `-dry-run` | Print events as JSON without sending |

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
