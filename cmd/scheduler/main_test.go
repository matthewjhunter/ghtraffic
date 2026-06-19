package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperProcess stands in for the real ghtraffic/ghpush binaries when the
// scheduler execs them under test. The STUB_NAME env var (set from the requested
// binary's basename) selects which behaviour to emulate.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("STUB_NAME") {
	case "ghtraffic":
		// Emulate the collector: emit one fresh NDJSON record to stdout, which
		// the scheduler redirects (appends) to the data file.
		fmt.Fprintln(os.Stdout, `{"repo":"matthewjhunter/x","date":"2026-06-19","views":{"count":1,"uniques":1}}`)
		if os.Getenv("STUB_FAIL") == "ghtraffic" {
			os.Exit(3)
		}
	case "ghpush":
		// Emulate the pusher: record exactly what it received on stdin so the
		// test can assert it got the appended data file.
		data, _ := io.ReadAll(os.Stdin)
		if p := os.Getenv("STUB_PUSH_OUT"); p != "" {
			_ = os.WriteFile(p, data, 0o644)
		}
		if os.Getenv("STUB_FAIL") == "ghpush" {
			os.Exit(4)
		}
	}
	os.Exit(0)
}

// installFakeExec points execCommand at this test binary's helper mode for the
// duration of the test.
func installFakeExec(t *testing.T) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		c := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		c.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"STUB_NAME="+filepath.Base(name),
		)
		return c
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestRunCycle_AppendsFetchOutputAndPipesToPush(t *testing.T) {
	installFakeExec(t)

	dir := t.TempDir()
	dataFile := filepath.Join(dir, "traffic.jsonl")
	pushOut := filepath.Join(dir, "ghpush-stdin.txt")
	existing := `{"repo":"matthewjhunter/old","date":"2026-06-01"}` + "\n"
	if err := os.WriteFile(dataFile, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_PUSH_OUT", pushOut)

	cfg := config{owner: "matthewjhunter", dataFile: dataFile, binDir: "/usr/local/bin"}
	if err := runCycle(context.Background(), cfg); err != nil {
		t.Fatalf("runCycle: %v", err)
	}

	// Data file should hold the pre-existing line plus the fetched stub line.
	got, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "matthewjhunter/old") {
		t.Error("existing data was lost")
	}
	if !strings.Contains(string(got), "matthewjhunter/x") {
		t.Error("fetched output was not appended to the data file")
	}

	// ghpush must have received the full (appended) data file on stdin.
	piped, err := os.ReadFile(pushOut)
	if err != nil {
		t.Fatalf("ghpush stub did not record stdin: %v", err)
	}
	if string(piped) != string(got) {
		t.Errorf("ghpush stdin != data file\n stdin=%q\n file =%q", piped, got)
	}
}

func TestRunCycle_FetchFailureSkipsPush(t *testing.T) {
	installFakeExec(t)
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "traffic.jsonl")
	pushOut := filepath.Join(dir, "ghpush-stdin.txt")
	t.Setenv("STUB_PUSH_OUT", pushOut)
	t.Setenv("STUB_FAIL", "ghtraffic")

	cfg := config{dataFile: dataFile, binDir: "/"}
	err := runCycle(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("expected fetch error, got %v", err)
	}
	if _, statErr := os.Stat(pushOut); statErr == nil {
		t.Error("ghpush ran despite fetch failure")
	}
}

func TestRunCycle_PushFailurePropagates(t *testing.T) {
	installFakeExec(t)
	dir := t.TempDir()
	dataFile := filepath.Join(dir, "traffic.jsonl")
	t.Setenv("STUB_FAIL", "ghpush")

	cfg := config{dataFile: dataFile, binDir: "/"}
	err := runCycle(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("expected push error, got %v", err)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	for _, k := range []string{"INTERVAL_SECONDS", "GHTRAFFIC_OWNER", "DATA_FILE", "BIN_DIR"} {
		t.Setenv(k, "")
	}
	cfg := loadConfig()
	if cfg.interval.Seconds() != 3600 {
		t.Errorf("interval = %v, want 3600s", cfg.interval)
	}
	if cfg.dataFile != "/data/traffic.jsonl" {
		t.Errorf("dataFile = %q", cfg.dataFile)
	}
	if cfg.binDir != "/" {
		t.Errorf("binDir = %q", cfg.binDir)
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	t.Setenv("INTERVAL_SECONDS", "60")
	t.Setenv("GHTRAFFIC_OWNER", "matthewjhunter")
	t.Setenv("DATA_FILE", "/tmp/t.jsonl")
	t.Setenv("BIN_DIR", "/opt/bin")
	cfg := loadConfig()
	if cfg.interval.Seconds() != 60 {
		t.Errorf("interval = %v, want 60s", cfg.interval)
	}
	if cfg.owner != "matthewjhunter" || cfg.dataFile != "/tmp/t.jsonl" || cfg.binDir != "/opt/bin" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}
