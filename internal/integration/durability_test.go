package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStateSurvivesARestart stops the service and starts a new one on the same
// directories, as happens on every deploy.
func TestStateSurvivesARestart(t *testing.T) {
	cfg := testConfig(t)
	first := startServer(t, cfg)

	first.createFlag("persisted", true)
	first.createFlag("also-persisted", false)
	first.setOverride("persisted", "vip", false)
	first.updateFlag("persisted", map[string]any{"description": "kept"})
	first.mustStatus(first.call("DELETE", "/v1/flags/also-persisted", "ops", nil), 204, "delete")
	first.expectEval("persisted", "vip", false, "user_override", "store")
	first.expectEval("persisted", "vip", false, "user_override", "cache")
	beforeAudit := first.audit("persisted")
	first.stop()

	second := startServer(t, cfg)
	got := decode[flagBody](t, second.mustStatus(second.call("GET", "/v1/flags/persisted", "", nil), 200, "get after restart"))
	if !got.Enabled || got.Description != "kept" || got.Version != 3 || got.Overrides["vip"] != false || len(got.Overrides) != 1 {
		t.Fatalf("flag after restart = %+v, want enabled, description kept, version 3, one override vip=false", got)
	}
	if r := second.call("GET", "/v1/flags/also-persisted", "", nil); r.Status != 404 {
		t.Fatalf("deleted flag came back after a restart: %d", r.Status)
	}

	// The cache is in memory only, so the first answer comes from the store and
	// must still be correct.
	second.expectEval("persisted", "vip", false, "user_override", "store")
	second.expectEval("persisted", "vip", false, "user_override", "cache")
	second.expectEval("persisted", "someone", true, "global_on", "store")

	afterAudit := second.audit("persisted")
	if len(afterAudit) != len(beforeAudit) {
		t.Fatalf("audit has %d entries after restart, had %d", len(afterAudit), len(beforeAudit))
	}
	for i := range afterAudit {
		if afterAudit[i].ID != beforeAudit[i].ID || afterAudit[i].Action != beforeAudit[i].Action || !afterAudit[i].At.Equal(beforeAudit[i].At) {
			t.Fatalf("audit[%d] changed across the restart: %+v vs %+v", i, afterAudit[i], beforeAudit[i])
		}
	}

	// Versions and history continue where they left off.
	next := second.updateFlag("persisted", map[string]any{"enabled": false})
	if next.Version != 4 {
		t.Fatalf("first update after restart has version %d, want 4", next.Version)
	}
	if h := second.audit("persisted"); len(h) != len(beforeAudit)+1 || h[0].Before.Version != 3 || h[0].After.Version != 4 {
		t.Fatalf("history did not continue: %d entries, newest %+v", len(h), h[0])
	}
	second.stop()

	if n := strings.Count(readFile(t, filepath.Join(cfg.LogDir, "app.log")), `"msg":"starting server"`); n != 2 {
		t.Fatalf("app.log shows %d starts, want 2 (the log file is appended, not replaced)", n)
	}
}

// TestStartsAfterACrashLeftDebrisOnDisk simulates the two ways a crash can
// leave files behind: a half-written audit line and an unfinished temp file
// from an interrupted flags.json update.
func TestStartsAfterACrashLeftDebrisOnDisk(t *testing.T) {
	cfg := testConfig(t)
	first := startServer(t, cfg)
	first.createFlag("survivor", true)
	first.setOverride("survivor", "u1", false)
	first.stop()

	auditPath := filepath.Join(cfg.DataDir, "audit.jsonl")
	f, err := os.OpenFile(auditPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"half","flag_name":"survivor","actor":"cra`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	temp := filepath.Join(cfg.DataDir, "flags.json.tmp-123456")
	if err := os.WriteFile(temp, []byte(`{"version":1,"flags":[{"name":"ghost`), 0o600); err != nil {
		t.Fatal(err)
	}

	second := startServer(t, cfg)
	if h := second.audit("survivor"); len(h) != 2 {
		t.Fatalf("history with a torn tail has %d entries, want the 2 complete ones", len(h))
	}
	if r := second.call("GET", "/v1/flags/ghost", "", nil); r.Status != 404 {
		t.Fatalf("an unfinished temp file leaked a flag: %d", r.Status)
	}
	second.expectEval("survivor", "u1", false, "user_override", "store")

	second.updateFlag("survivor", map[string]any{"enabled": false})
	h := second.audit("survivor")
	if len(h) != 3 || h[0].Action != "flag.updated" {
		t.Fatalf("a write after the torn tail was not recorded cleanly: %d entries, newest %+v", len(h), h[0])
	}
	second.stop()

	third := startServer(t, cfg)
	if h := third.audit("survivor"); len(h) != 3 {
		t.Fatalf("history after a third start has %d entries, want 3", len(h))
	}
}
