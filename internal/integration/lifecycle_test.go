package integration

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlagLifecycleOnRealFiles walks a flag through its whole life on the
// production wiring and checks what ends up on disk.
func TestFlagLifecycleOnRealFiles(t *testing.T) {
	cfg := testConfig(t)
	s := startServer(t, cfg)
	const flag = "checkout-v2"

	// A validation failure is a 400 and leaves no trace behind.
	for name, body := range map[string]any{
		"name with a space":  map[string]any{"name": "bad name"},
		"empty name":         map[string]any{"name": ""},
		"name over 64 chars": map[string]any{"name": strings.Repeat("a", 65)},
	} {
		r := s.call("POST", "/v1/flags", "alice", body)
		if r.Status != 400 || decode[errorBody](t, r).Error.Code != "invalid_request" {
			t.Fatalf("%s: got %d %s, want 400 invalid_request", name, r.Status, r)
		}
	}
	if r := s.call("POST", "/v1/flags", "", map[string]any{"name": flag}); r.Status != 400 ||
		decode[errorBody](t, r).Error.Code != "missing_actor" {
		t.Fatalf("create without an actor: got %d %s", r.Status, r)
	}
	if r := s.call("GET", "/v1/flags", "", nil); len(decode[struct{ Flags []flagBody }](t, r).Flags) != 0 {
		t.Fatalf("rejected requests created flags: %s", r)
	}

	created := s.createFlag(flag, false)
	if created.Enabled || created.Version != 1 {
		t.Fatalf("new flag = %+v, want disabled at version 1", created)
	}
	if r := s.call("POST", "/v1/flags", "alice", map[string]any{"name": flag}); r.Status != 409 {
		t.Fatalf("duplicate create = %d, want 409", r.Status)
	}

	s.expectEval(flag, "u1", false, "global_off", "store")

	s.setOverride(flag, "u1", true)
	s.expectEval(flag, "u1", true, "user_override", "store")
	s.expectEval(flag, "u1", true, "user_override", "cache")
	s.expectEval(flag, "u2", false, "global_off", "store")

	s.updateFlag(flag, map[string]any{"enabled": true})
	s.expectEval(flag, "u2", true, "global_on", "store")
	s.expectEval(flag, "u2", true, "global_on", "cache")

	s.updateFlag(flag, map[string]any{"enabled": false, "description": "rolled back"})
	s.expectEval(flag, "u2", false, "global_off", "store")
	s.expectEval(flag, "u1", true, "user_override", "store")

	s.mustStatus(s.call("DELETE", "/v1/flags/"+flag+"/overrides/u1", "alice", nil), 204, "remove override")
	s.expectEval(flag, "u1", false, "global_off", "store")

	history := s.audit(flag)
	wantActions := []string{"override.removed", "flag.updated", "flag.updated", "override.set", "flag.created"}
	if len(history) != len(wantActions) {
		t.Fatalf("audit has %d entries, want %d: %+v", len(history), len(wantActions), history)
	}
	for i, e := range history {
		if e.Action != wantActions[i] {
			t.Errorf("audit[%d].action = %s, want %s", i, e.Action, wantActions[i])
		}
		if e.At.IsZero() || e.ID == "" || e.FlagName != flag {
			t.Errorf("audit[%d] is missing id, time or flag name: %+v", i, e)
		}
	}
	oldest := history[len(history)-1]
	if oldest.Before != nil || oldest.After == nil || oldest.Actor != "tester" {
		t.Errorf("creation entry = %+v, want no 'before', an 'after', actor tester", oldest)
	}
	if e := history[0]; e.Actor != "alice" || e.Before == nil || e.After == nil || !(e.After.Version == e.Before.Version+1) {
		t.Errorf("latest entry does not link versions: %+v", e)
	}

	s.mustStatus(s.call("DELETE", "/v1/flags/"+flag, "alice", nil), 204, "delete flag")
	for _, p := range []string{"/v1/flags/" + flag, "/v1/evaluate?flag=" + flag + "&user_id=u1"} {
		r := s.call("GET", p, "", nil)
		if r.Status != 404 || decode[errorBody](t, r).Error.Code != "not_found" {
			t.Fatalf("GET %s after delete = %d %s, want 404 not_found", p, r.Status, r)
		}
	}
	if r := s.call("DELETE", "/v1/flags/"+flag, "alice", nil); r.Status != 404 {
		t.Fatalf("second delete = %d, want 404", r.Status)
	}
	history = s.audit(flag)
	if len(history) != 6 || history[0].Action != "flag.deleted" || history[0].After != nil || history[0].Before == nil {
		t.Fatalf("history after delete = %d entries, newest %+v; want 6 ending in flag.deleted", len(history), history[0])
	}

	s.stop()

	flagsOnDisk := readFile(t, filepath.Join(cfg.DataDir, "flags.json"))
	if strings.Contains(flagsOnDisk, flag) {
		t.Errorf("deleted flag is still in flags.json:\n%s", flagsOnDisk)
	}
	lines := nonEmptyLines(readFile(t, filepath.Join(cfg.DataDir, "audit.jsonl")))
	if len(lines) != 6 {
		t.Fatalf("audit.jsonl has %d lines, want one per write (6)", len(lines))
	}
	for i, l := range lines {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Errorf("audit.jsonl line %d is not JSON: %v", i+1, err)
		}
	}
}

// TestEveryRequestIsTraceableThroughLogsAndAudit follows one request id from
// the client, through the access log, into the audit record.
func TestEveryRequestIsTraceableThroughLogsAndAudit(t *testing.T) {
	cfg := testConfig(t)
	s := startServer(t, cfg)

	const rid = "trace-1234.abc"
	r := s.call("POST", "/v1/flags", "carol", map[string]any{"name": "traced"}, "X-Request-ID", rid)
	s.mustStatus(r, 201, "create")
	if got := r.Header.Get("X-Request-ID"); got != rid {
		t.Fatalf("response request id = %q, want it echoed", got)
	}

	generated := s.call("GET", "/v1/flags/traced", "", nil).Header.Get("X-Request-ID")
	if len(generated) < 8 {
		t.Fatalf("no request id generated for a request without one: %q", generated)
	}

	hostile := s.call("GET", "/healthz", "", nil, "X-Request-ID", "bad id\twith spaces")
	if got := hostile.Header.Get("X-Request-ID"); got == "bad id\twith spaces" || got == "" {
		t.Fatalf("an unsafe client request id was reused: %q", got)
	}

	entries := s.audit("traced")
	if len(entries) != 1 || entries[0].RequestID != rid || entries[0].Actor != "carol" {
		t.Fatalf("audit = %+v, want one entry from carol carrying request id %s", entries, rid)
	}

	s.stop()

	var access map[string]any
	for _, rec := range logRecords(t, filepath.Join(cfg.LogDir, "app.log")) {
		if rec["request_id"] == rid && rec["msg"] == "request completed" {
			access = rec
		}
	}
	if access == nil {
		t.Fatalf("no access-log line carries request id %s", rid)
	}
	if access["actor"] != "carol" || access["method"] != "POST" || access["path"] != "/v1/flags" || access["status"] != float64(201) {
		t.Errorf("access log line = %v, want actor carol, POST /v1/flags, status 201", access)
	}
	ts, err := time.Parse(time.RFC3339Nano, access["time"].(string))
	if err != nil || time.Since(ts) > time.Minute {
		t.Errorf("access log time %v is not a recent RFC 3339 timestamp (%v)", access["time"], err)
	}
	if access["level"] != "info" {
		t.Errorf("a successful request logged at %v, want info", access["level"])
	}

	if _, err := os.Stat(filepath.Join(cfg.LogDir, "error.log")); err == nil {
		if recs := logRecords(t, filepath.Join(cfg.LogDir, "error.log")); len(recs) != 0 {
			t.Errorf("a clean run wrote %d lines to error.log: %v", len(recs), recs)
		}
	}
}

// TestClientErrorsAreWarningsNotErrors checks the level policy: a caller's
// mistake (4xx) is a warning, so error.log, which collects warn and error
// records for triage, can still be scanned for real server faults (5xx and
// panics), which are the only records at level error.
func TestClientErrorsAreWarningsNotErrors(t *testing.T) {
	cfg := testConfig(t)
	s := startServer(t, cfg)
	s.call("GET", "/v1/flags/missing", "", nil)
	s.call("POST", "/v1/flags", "alice", map[string]any{"name": "bad name"})
	s.stop()

	seen := map[float64]bool{}
	for _, rec := range logRecords(t, filepath.Join(cfg.LogDir, "app.log")) {
		status, ok := rec["status"].(float64)
		if !ok || (status != 404 && status != 400) {
			continue
		}
		seen[status] = true
		if rec["level"] != "warn" {
			t.Errorf("a %v was logged at %v, want warn: %v", status, rec["level"], rec)
		}
	}
	if !seen[404] || !seen[400] {
		t.Fatalf("access log lacks the failed requests: %v", seen)
	}

	for _, rec := range logRecords(t, filepath.Join(cfg.LogDir, "error.log")) {
		if rec["level"] == "error" {
			t.Errorf("client mistakes produced an error-level record: %v", rec)
		}
		if rec["level"] != "warn" {
			t.Errorf("error.log holds a %v record, want only warn and above", rec["level"])
		}
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func logRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, l := range nonEmptyLines(readFile(t, path)) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("%s has a line that is not JSON: %q", filepath.Base(path), l)
		}
		recs = append(recs, rec)
	}
	return recs
}
