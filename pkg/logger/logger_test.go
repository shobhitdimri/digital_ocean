package logger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type record map[string]any

func readRecords(t *testing.T, path string) []record {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("invalid JSON line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func levels(recs []record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r["level"].(string))
	}
	return out
}

func logAllLevels(l *Logger) {
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
}

func newTestLogger(t *testing.T, level string) (*Logger, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	stdout := &bytes.Buffer{}
	l, err := New(Config{Level: level, Dir: dir, MaxSizeMB: 1, Stdout: stdout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir, stdout
}

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		level    string
		wantApp  []string
		wantErrs []string
	}{
		{"debug", []string{"debug", "info", "warn", "error"}, []string{"warn", "error"}},
		{"info", []string{"info", "warn", "error"}, []string{"warn", "error"}},
		{"warn", []string{"warn", "error"}, []string{"warn", "error"}},
		{"error", []string{"error"}, []string{"error"}},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			l, dir, stdout := newTestLogger(t, tc.level)
			logAllLevels(l)
			if err := l.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			app := readRecords(t, filepath.Join(dir, "app.log"))
			if got := levels(app); !reflect.DeepEqual(got, tc.wantApp) {
				t.Errorf("app.log levels = %v, want %v", got, tc.wantApp)
			}
			errs := readRecords(t, filepath.Join(dir, "error.log"))
			if got := levels(errs); !reflect.DeepEqual(got, tc.wantErrs) {
				t.Errorf("error.log levels = %v, want %v", got, tc.wantErrs)
			}
			if got := strings.Count(stdout.String(), "\n"); got != len(tc.wantApp) {
				t.Errorf("stdout lines = %d, want %d", got, len(tc.wantApp))
			}
		})
	}
}

func TestRecordShape(t *testing.T) {
	l, dir, _ := newTestLogger(t, "info")
	l.Info("hello", "key", "value")
	_ = l.Close()

	recs := readRecords(t, filepath.Join(dir, "app.log"))
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	for _, k := range []string{"time", "level", "msg", "service", "key"} {
		if _, ok := r[k]; !ok {
			t.Errorf("record missing %q: %v", k, r)
		}
	}
	if r["service"] != "feature-flag-service" || r["msg"] != "hello" || r["level"] != "info" {
		t.Errorf("unexpected record: %v", r)
	}
}

func TestRequestScopedContext(t *testing.T) {
	l, dir, _ := newTestLogger(t, "info")

	ctx := WithRequest(context.Background(), l.Logger, "req-123", "alice")
	FromContext(ctx).Info("flag updated", "flag", "new-ui")
	if RequestID(ctx) != "req-123" || Actor(ctx) != "alice" {
		t.Fatalf("accessors: request_id=%q actor=%q", RequestID(ctx), Actor(ctx))
	}

	anon := WithRequest(context.Background(), l.Logger, "req-456", "")
	FromContext(anon).Info("evaluated")
	if Actor(anon) != AnonymousActor {
		t.Fatalf("empty actor should become %q, got %q", AnonymousActor, Actor(anon))
	}
	_ = l.Close()

	recs := readRecords(t, filepath.Join(dir, "app.log"))
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0]["request_id"] != "req-123" || recs[0]["actor"] != "alice" || recs[0]["flag"] != "new-ui" {
		t.Errorf("first record missing request fields: %v", recs[0])
	}
	if recs[1]["request_id"] != "req-456" || recs[1]["actor"] != AnonymousActor {
		t.Errorf("second record missing request fields: %v", recs[1])
	}
}

func TestFromContextFallback(t *testing.T) {
	if FromContext(context.Background()) == nil {
		t.Fatal("FromContext must never return nil")
	}
	if RequestID(context.Background()) != "" || Actor(context.Background()) != "" {
		t.Fatal("empty context should have no request identity")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{" warn ", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"", slog.LevelInfo, false},
		{"trace", 0, true},
	}
	for _, tc := range tests {
		got, err := ParseLevel(tc.in)
		if (err != nil) != tc.wantErr || (!tc.wantErr && got != tc.want) {
			t.Errorf("ParseLevel(%q) = %v, %v", tc.in, got, err)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Level: "loud", Dir: t.TempDir()}); err == nil {
		t.Error("want error for unknown level")
	}
	if _, err := New(Config{Level: "info"}); err == nil {
		t.Error("want error for empty dir")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	l, _, _ := newTestLogger(t, "info")
	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
