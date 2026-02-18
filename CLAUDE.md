# ghtraffic

CLI tool that collects GitHub traffic data (views, clones, referrers, popular paths) for all repositories the authenticated user has push access to, including organization repos. Outputs newline-delimited JSON to stdout, one record per repo per day. Designed to run daily via cron.

Authentication uses `GITHUB_TOKEN` env var, falling back to `gh auth token`.

## Build

```bash
go build -o ghtraffic .
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
