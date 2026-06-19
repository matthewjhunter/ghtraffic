package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperProcess stands in for the real ghtraffic/ghpush binaries when the
// scheduler execs them under test. It distinguishes the two by their args: the
// ghtraffic invocation carries "-seen" (and optionally "-owner"); ghpush has no
// args and reads stdin.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	var rest []string
	for i, a := range os.Args {
		if a == "--" {
			rest = os.Args[i+1:]
			break
		}
	}

	isFetch := false
	owner := ""
	for i, a := range rest {
		if a == "-seen" {
			isFetch = true
		}
		if a == "-owner" && i+1 < len(rest) {
			owner = rest[i+1]
		}
	}

	if isFetch {
		// ghtraffic stub: emit one record echoing the owner and the token it ran
		// with, so the test can assert per-owner token routing.
		if os.Getenv("STUB_FAIL_OWNER") == owner {
			os.Exit(3)
		}
		fmt.Printf(`{"owner_seen":%q,"token_seen":%q}`+"\n", owner, os.Getenv("GITHUB_TOKEN"))
		os.Exit(0)
	}

	// ghpush stub: record stdin so the test can assert it received the data file.
	data, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("STUB_PUSH_OUT"); p != "" {
		_ = os.WriteFile(p, data, 0o644)
	}
	if os.Getenv("STUB_FAIL") == "ghpush" {
		os.Exit(4)
	}
	os.Exit(0)
}

// installFakeExec points execCommand at this test binary's helper mode. The
// helper is activated via GO_WANT_HELPER_PROCESS, which flows to the child
// through the inherited environment.
func installFakeExec(t *testing.T) {
	t.Helper()
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		return exec.CommandContext(ctx, os.Args[0], helperArgs...)
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestRunCycle_MultiOwnerRoutesTokensAndPipesToPush(t *testing.T) {
	installFakeExec(t)
	dir := t.TempDir()
	dataFile := dir + "/traffic.jsonl"
	pushOut := dir + "/ghpush-stdin.txt"
	t.Setenv("STUB_PUSH_OUT", pushOut)

	cfg := config{
		dataFile: dataFile,
		binDir:   "/usr/local/bin",
		owners: []ownerSpec{
			{name: "matthewjhunter", token: "tok-mjh"},
			{name: "infodancer", token: "tok-inf"},
		},
	}
	if err := runCycle(context.Background(), cfg); err != nil {
		t.Fatalf("runCycle: %v", err)
	}

	got, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	// Each owner must have been collected with its own token.
	if !strings.Contains(s, `"owner_seen":"matthewjhunter","token_seen":"tok-mjh"`) {
		t.Errorf("matthewjhunter not collected with its token; file=%s", s)
	}
	if !strings.Contains(s, `"owner_seen":"infodancer","token_seen":"tok-inf"`) {
		t.Errorf("infodancer not collected with its token; file=%s", s)
	}

	piped, err := os.ReadFile(pushOut)
	if err != nil {
		t.Fatalf("ghpush did not receive stdin: %v", err)
	}
	if string(piped) != s {
		t.Errorf("ghpush stdin != data file")
	}
}

func TestRunCycle_OneOwnerFailsStillPushesOthers(t *testing.T) {
	installFakeExec(t)
	dir := t.TempDir()
	dataFile := dir + "/traffic.jsonl"
	pushOut := dir + "/ghpush-stdin.txt"
	t.Setenv("STUB_PUSH_OUT", pushOut)
	t.Setenv("STUB_FAIL_OWNER", "infodancer")

	cfg := config{
		dataFile: dataFile,
		binDir:   "/",
		owners: []ownerSpec{
			{name: "matthewjhunter", token: "tok-mjh"},
			{name: "infodancer", token: "tok-inf"},
		},
	}
	err := runCycle(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "infodancer") {
		t.Fatalf("expected error mentioning failed owner, got %v", err)
	}
	// The good owner's data is still collected and the push still runs.
	got, _ := os.ReadFile(dataFile)
	if !strings.Contains(string(got), "matthewjhunter") {
		t.Error("good owner was not collected")
	}
	if _, statErr := os.Stat(pushOut); statErr != nil {
		t.Error("push was skipped despite only one owner failing")
	}
}

func TestRunCycle_PushFailurePropagates(t *testing.T) {
	installFakeExec(t)
	dir := t.TempDir()
	t.Setenv("STUB_FAIL", "ghpush")

	cfg := config{dataFile: dir + "/traffic.jsonl", binDir: "/", owners: []ownerSpec{{name: "x", token: "t"}}}
	err := runCycle(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("expected push error, got %v", err)
	}
}

func TestLoadOwners_MultiOwner(t *testing.T) {
	t.Setenv("GHTRAFFIC_OWNERS", "matthewjhunter, infodancer ,old-school-gamers")
	t.Setenv("GHTRAFFIC_TOKEN_MATTHEWJHUNTER", "a")
	t.Setenv("GHTRAFFIC_TOKEN_INFODANCER", "b")
	t.Setenv("GHTRAFFIC_TOKEN_OLD_SCHOOL_GAMERS", "c")
	got := loadOwners()
	want := []ownerSpec{{"matthewjhunter", "a"}, {"infodancer", "b"}, {"old-school-gamers", "c"}}
	if len(got) != len(want) {
		t.Fatalf("got %d owners, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("owner %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadOwners_SkipsOwnerWithoutToken(t *testing.T) {
	t.Setenv("GHTRAFFIC_OWNERS", "matthewjhunter,infodancer")
	t.Setenv("GHTRAFFIC_TOKEN_MATTHEWJHUNTER", "a")
	t.Setenv("GHTRAFFIC_TOKEN_INFODANCER", "") // missing
	got := loadOwners()
	if len(got) != 1 || got[0].name != "matthewjhunter" {
		t.Errorf("expected only matthewjhunter, got %+v", got)
	}
}

func TestLoadOwners_SingleOwnerFallback(t *testing.T) {
	t.Setenv("GHTRAFFIC_OWNERS", "")
	t.Setenv("GHTRAFFIC_OWNER", "matthewjhunter")
	t.Setenv("GITHUB_TOKEN", "tok")
	got := loadOwners()
	if len(got) != 1 || got[0].name != "matthewjhunter" || got[0].token != "tok" {
		t.Errorf("single-owner fallback wrong: %+v", got)
	}
}

func TestTokenEnvSuffix(t *testing.T) {
	cases := map[string]string{
		"matthewjhunter":     "MATTHEWJHUNTER",
		"old-school-gamers":  "OLD_SCHOOL_GAMERS",
		"speculativefiction": "SPECULATIVEFICTION",
	}
	for in, want := range cases {
		if got := tokenEnvSuffix(in); got != want {
			t.Errorf("tokenEnvSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	for _, k := range []string{"INTERVAL_SECONDS", "DATA_FILE", "BIN_DIR", "GHTRAFFIC_OWNERS", "GHTRAFFIC_OWNER", "GITHUB_TOKEN"} {
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
