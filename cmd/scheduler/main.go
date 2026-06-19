// scheduler is the long-running entrypoint for the ghtraffic container. On an
// hourly ticker (and once immediately on start) it runs one collect+push cycle:
// it execs ghtraffic to append fresh GitHub traffic to the data file, then execs
// ghpush to send the new deltas to Umami. It is the only long-lived process in
// the image; ghtraffic and ghpush stay one-shot CLIs.
//
// Configuration is entirely via environment variables:
//
//	INTERVAL_SECONDS  cycle period in seconds (default 3600)
//	GHTRAFFIC_OWNER   restrict collection to this owner/org (optional)
//	DATA_FILE         NDJSON traffic history file (default /data/traffic.jsonl)
//	BIN_DIR           directory holding the ghtraffic and ghpush binaries (default /)
//
// ghtraffic and ghpush read their own env (GITHUB_TOKEN, UMAMI_URL,
// UMAMI_WEBSITE_ID, GHPUSH_DATABASE_URL); the scheduler passes the environment
// through unchanged.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// execCommand is indirected so tests can substitute the real binaries with
// in-process stubs.
var execCommand = exec.CommandContext

type config struct {
	interval time.Duration
	owner    string
	dataFile string
	binDir   string
}

func loadConfig() config {
	interval := 3600
	if v := os.Getenv("INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		} else {
			log.Printf("invalid INTERVAL_SECONDS %q, using %d", v, interval)
		}
	}
	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "/data/traffic.jsonl"
	}
	binDir := os.Getenv("BIN_DIR")
	if binDir == "" {
		binDir = "/"
	}
	return config{
		interval: time.Duration(interval) * time.Second,
		owner:    os.Getenv("GHTRAFFIC_OWNER"),
		dataFile: dataFile,
		binDir:   binDir,
	}
}

// runCycle performs one collect-then-push cycle. A fetch failure short-circuits
// the push (no point pushing if collection failed); both errors are wrapped so
// the caller can log them without aborting the loop.
func runCycle(ctx context.Context, cfg config) error {
	if err := runFetch(ctx, cfg); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := runPush(ctx, cfg); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// runFetch execs ghtraffic, appending its NDJSON output to the data file. This
// mirrors the legacy cron `ghtraffic -seen F >> F`: ghtraffic loads the seen set
// fully before emitting, so appending to the same file it reads is safe.
func runFetch(ctx context.Context, cfg config) error {
	f, err := os.OpenFile(cfg.dataFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	defer f.Close()

	args := []string{"-seen", cfg.dataFile}
	if cfg.owner != "" {
		args = append(args, "-owner", cfg.owner)
	}
	cmd := execCommand(ctx, filepath.Join(cfg.binDir, "ghtraffic"), args...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runPush execs ghpush with the data file on stdin. ghpush computes deltas
// against its Postgres push-state and posts them to Umami.
func runPush(ctx context.Context, cfg config) error {
	in, err := os.Open(cfg.dataFile)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	defer in.Close()

	cmd := execCommand(ctx, filepath.Join(cfg.binDir, "ghpush"))
	cmd.Stdin = in
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("scheduler start: interval=%s owner=%q data=%s", cfg.interval, cfg.owner, cfg.dataFile)

	runOnce := func() {
		start := time.Now()
		if err := runCycle(ctx, cfg); err != nil {
			log.Printf("cycle error: %v", err)
			return
		}
		log.Printf("cycle ok (%s)", time.Since(start).Round(time.Millisecond))
	}

	runOnce() // immediate run so a fresh container catches up without waiting a full interval

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("shutdown signal received, exiting")
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
