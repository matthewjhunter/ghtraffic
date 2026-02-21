package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSeen_ExcludesToday(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	path := filepath.Join(t.TempDir(), "seen.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	enc := json.NewEncoder(f)
	enc.Encode(map[string]string{"repo": "a/b", "date": yesterday})
	enc.Encode(map[string]string{"repo": "a/b", "date": today})
	f.Close()

	seen, err := loadSeen(path)
	if err != nil {
		t.Fatalf("loadSeen: %v", err)
	}
	if !seen["a/b|"+yesterday] {
		t.Errorf("expected yesterday (%s) in seen set", yesterday)
	}
	if seen["a/b|"+today] {
		t.Errorf("today (%s) must not be in seen set — should always be re-fetched", today)
	}
}

func TestLoadSeen_EmptyPath(t *testing.T) {
	seen, err := loadSeen("")
	if err != nil {
		t.Fatalf("loadSeen: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("expected empty seen set for empty path, got %v", seen)
	}
}

func TestLoadSeen_MissingFile(t *testing.T) {
	seen, err := loadSeen(filepath.Join(t.TempDir(), "nonexistent.jsonl"))
	if err != nil {
		t.Fatalf("loadSeen: %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("expected empty seen set for missing file, got %v", seen)
	}
}

func TestLoadSeen_SkipsMalformedLines(t *testing.T) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	path := filepath.Join(t.TempDir(), "seen.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.WriteString("not json\n")
	enc := json.NewEncoder(f)
	enc.Encode(map[string]string{"repo": "a/b", "date": yesterday})
	f.Close()

	seen, err := loadSeen(path)
	if err != nil {
		t.Fatalf("loadSeen: %v", err)
	}
	if !seen["a/b|"+yesterday] {
		t.Errorf("expected valid record to be in seen set")
	}
}
