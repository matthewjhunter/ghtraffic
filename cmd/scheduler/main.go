// scheduler is the long-running entrypoint for the ghtraffic container. On an
// hourly ticker (and once immediately on start) it runs one collect+push cycle:
// for each configured owner it execs ghtraffic with that owner's token,
// appending fresh GitHub traffic to the data file, then execs ghpush once to
// send the new deltas to Umami. ghtraffic and ghpush stay one-shot CLIs.
//
// Configuration is entirely via environment variables:
//
//	INTERVAL_SECONDS  cycle period in seconds (default 3600)
//	DATA_FILE         NDJSON traffic history file (default /data/traffic.jsonl)
//	BIN_DIR           directory holding the ghtraffic and ghpush binaries (default /)
//
// Owners and tokens (multi-owner mode):
//
//	GHTRAFFIC_OWNERS         comma-separated owners, e.g. "matthewjhunter,infodancer"
//	GHTRAFFIC_TOKEN_<OWNER>  the PAT for each owner; <OWNER> is the owner
//	                         uppercased with non-alphanumerics replaced by "_"
//	                         (e.g. old-school-gamers -> GHTRAFFIC_TOKEN_OLD_SCHOOL_GAMERS)
//
// Single-owner fallback (used when GHTRAFFIC_OWNERS is unset):
//
//	GHTRAFFIC_OWNER  restrict collection to this owner/org (optional)
//	GITHUB_TOKEN     the PAT used for collection
//
// ghpush reads its own env (UMAMI_URL, UMAMI_WEBSITE_ID, GHPUSH_DATABASE_URL).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// execCommand is indirected so tests can substitute the real binaries with
// in-process stubs.
var execCommand = exec.CommandContext

// ownerSpec pairs an owner/org with the token used to collect its traffic.
type ownerSpec struct {
	name  string
	token string
}

type config struct {
	interval time.Duration
	owners   []ownerSpec
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
		owners:   loadOwners(),
		dataFile: dataFile,
		binDir:   binDir,
	}
}

// loadOwners resolves the owner/token list. Multi-owner mode reads
// GHTRAFFIC_OWNERS and a per-owner GHTRAFFIC_TOKEN_<OWNER>; if that is unset it
// falls back to the single-owner GHTRAFFIC_OWNER + GITHUB_TOKEN pair.
func loadOwners() []ownerSpec {
	list := strings.TrimSpace(os.Getenv("GHTRAFFIC_OWNERS"))
	if list == "" {
		return []ownerSpec{{name: os.Getenv("GHTRAFFIC_OWNER"), token: os.Getenv("GITHUB_TOKEN")}}
	}
	var owners []ownerSpec
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		suffix := tokenEnvSuffix(name)
		tok := os.Getenv("GHTRAFFIC_TOKEN_" + suffix)
		if tok == "" {
			log.Printf("warning: no token for owner %q (set GHTRAFFIC_TOKEN_%s), skipping", name, suffix)
			continue
		}
		owners = append(owners, ownerSpec{name: name, token: tok})
	}
	return owners
}

// tokenEnvSuffix maps an owner name to the GHTRAFFIC_TOKEN_ env var suffix:
// uppercased, with every non-alphanumeric rune replaced by "_".
func tokenEnvSuffix(owner string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(owner) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// runCycle performs one collect-then-push cycle. ghtraffic runs once per owner;
// a single owner's failure is recorded but does not skip the push (we still want
// to send whatever was collected). All errors are joined for the caller to log.
func runCycle(ctx context.Context, cfg config) error {
	var errs []error
	for _, sp := range cfg.owners {
		if err := runFetch(ctx, cfg, sp); err != nil {
			errs = append(errs, fmt.Errorf("fetch %q: %w", sp.name, err))
		}
	}
	if err := runPush(ctx, cfg); err != nil {
		errs = append(errs, fmt.Errorf("push: %w", err))
	}
	return errors.Join(errs...)
}

// runFetch execs ghtraffic for one owner, appending its NDJSON output to the data
// file. This mirrors the legacy cron `ghtraffic -seen F >> F`: ghtraffic loads the
// seen set fully before emitting, so appending to the file it reads is safe.
func runFetch(ctx context.Context, cfg config, sp ownerSpec) error {
	f, err := os.OpenFile(cfg.dataFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	defer f.Close()

	args := []string{"-seen", cfg.dataFile}
	if sp.name != "" {
		args = append(args, "-owner", sp.name)
	}
	cmd := execCommand(ctx, filepath.Join(cfg.binDir, "ghtraffic"), args...)
	cmd.Env = envWithToken(sp.token)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// envWithToken returns the current environment with GITHUB_TOKEN set to token
// (replacing any inherited value), so each owner is collected with its own PAT.
func envWithToken(token string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GITHUB_TOKEN="+token)
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

func ownerNames(owners []ownerSpec) []string {
	names := make([]string, len(owners))
	for i, sp := range owners {
		names[i] = sp.name
	}
	return names
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := loadConfig()
	if len(cfg.owners) == 0 {
		log.Print("warning: no owners configured; nothing will be collected")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("scheduler start: interval=%s owners=%v data=%s", cfg.interval, ownerNames(cfg.owners), cfg.dataFile)

	runOnce := func() {
		start := time.Now()
		if err := runCycle(ctx, cfg); err != nil {
			log.Printf("cycle completed with errors (%s): %v", time.Since(start).Round(time.Millisecond), err)
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
