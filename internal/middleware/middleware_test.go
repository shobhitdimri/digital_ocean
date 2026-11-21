package middleware_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"feature-flag-service/internal/middleware"
	"feature-flag-service/pkg/httpx"
	"feature-flag-service/pkg/logger"
)

type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func newLog() (*slog.Logger, *logBuf) {
	b := &logBuf{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

type seqIDs struct{ n atomic.Int64 }

func (s *seqIDs) NewID() string { return fmt.Sprintf("gen-%d", s.n.Add(1)) }

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func do(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestChainOrderFirstIsOutermost(t *testing.T) {
	var order []string
	mk := func(name string) middleware.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":in")
				next.ServeHTTP(w, r)
				order = append(order, name+":out")
			})
		}
	}
	h := middleware.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mk("a"), mk("b"), mk("c"))
	do(h, "GET", "/", nil)

	want := "a:in b:in c:in handler c:out b:out a:out"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestChainWithNoMiddlewareReturnsHandler(t *testing.T) {
	called := false
	middleware.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("handler not called")
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		supplied string
		want     string
	}{
		{"generated when absent", "", "gen-1"},
		{"client id is reused", "client-req_1.A", "client-req_1.A"},
		{"max length is accepted", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"too long is replaced", strings.Repeat("a", 65), "gen-1"},
		{"spaces are replaced", "has space", "gen-1"},
		{"newline injection is replaced", "abc\nlevel=ERROR forged", "gen-1"},
		{"unicode is replaced", "idé", "gen-1"},
		{"slash is replaced", "a/b", "gen-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := newLog()
			var inCtx string
			h := middleware.RequestID(base, &seqIDs{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inCtx = logger.RequestID(r.Context())
			}))
			headers := map[string]string{}
			if tt.supplied != "" {
				headers[middleware.RequestIDHeader] = tt.supplied
			}
			rec := do(h, "GET", "/", headers)

			if got := rec.Header().Get(middleware.RequestIDHeader); got != tt.want {
				t.Errorf("response header = %q, want %q", got, tt.want)
			}
			if inCtx != tt.want {
				t.Errorf("context id = %q, want %q", inCtx, tt.want)
			}
		})
	}
}

func TestEachRequestGetsADistinctGeneratedID(t *testing.T) {
	base, _ := newLog()
	h := middleware.RequestID(base, &seqIDs{})(http.HandlerFunc(ok))
	a := do(h, "GET", "/", nil).Header().Get(middleware.RequestIDHeader)
	b := do(h, "GET", "/", nil).Header().Get(middleware.RequestIDHeader)
	if a == b {
		t.Fatalf("both requests got %q", a)
	}
}

func TestActor(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		actor      *string
		wantStatus int
		wantCode   string
		wantActor  string
	}{
		{"GET without header is anonymous", "GET", nil, 200, "", "anonymous"},
		{"POST without header is rejected", "POST", nil, 400, "missing_actor", ""},
		{"PUT without header is rejected", "PUT", nil, 400, "missing_actor", ""},
		{"PATCH without header is rejected", "PATCH", nil, 400, "missing_actor", ""},
		{"DELETE without header is rejected", "DELETE", nil, 400, "missing_actor", ""},
		{"blank header on write is rejected", "POST", ptr("   "), 400, "missing_actor", ""},
		{"POST with actor", "POST", ptr("alice"), 200, "", "alice"},
		{"GET with actor", "GET", ptr("bob"), 200, "", "bob"},
		{"surrounding spaces are trimmed", "PUT", ptr("  carol  "), 200, "", "carol"},
		{"actor with inner space is invalid", "POST", ptr("not valid"), 400, "invalid_actor", ""},
		{"invalid actor is rejected on GET too", "GET", ptr("not valid"), 400, "invalid_actor", ""},
		{"overlong actor is invalid", "POST", ptr(strings.Repeat("a", 129)), 400, "invalid_actor", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _ := newLog()
			var seen string
			h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = logger.Actor(r.Context())
			}), middleware.RequestID(base, &seqIDs{}), middleware.Actor(base))

			headers := map[string]string{}
			if tt.actor != nil {
				headers[middleware.ActorHeader] = *tt.actor
			}
			rec := do(h, tt.method, "/", headers)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantCode != "" {
				var body httpx.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != tt.wantCode || body.Error.RequestID != "gen-1" {
					t.Fatalf("body = %+v", body)
				}
				if seen != "" {
					t.Fatal("handler ran despite the rejection")
				}
				return
			}
			if seen != tt.wantActor {
				t.Fatalf("actor in context = %q, want %q", seen, tt.wantActor)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestRequestScopedLoggerCarriesRequestIDAndActor(t *testing.T) {
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.FromContext(r.Context()).Info("from deep in the call chain")
	}), middleware.RequestID(base, &seqIDs{}), middleware.Actor(base))
	do(h, "POST", "/", map[string]string{middleware.ActorHeader: "alice", middleware.RequestIDHeader: "req-7"})

	recs := logs.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	if recs[0]["request_id"] != "req-7" || recs[0]["actor"] != "alice" {
		t.Fatalf("record = %v", recs[0])
	}
}

func accessLog(t *testing.T, slow time.Duration, status int, method, target string, delay time.Duration) map[string]any {
	t.Helper()
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(status)
		io.WriteString(w, "12345")
	}), middleware.RequestID(base, &seqIDs{}), middleware.Actor(base), middleware.Logging(slow))
	do(h, method, target, map[string]string{middleware.ActorHeader: "alice"})

	recs := logs.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	return recs[0]
}

func TestAccessLogFieldsAndLevels(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		target    string
		wantLevel string
	}{
		{"200 is INFO", 200, "/v1/flags", "INFO"},
		{"201 is INFO", 201, "/v1/flags", "INFO"},
		{"404 is WARN", 404, "/v1/flags/x", "WARN"},
		{"400 is WARN", 400, "/v1/flags", "WARN"},
		{"500 is ERROR", 500, "/v1/flags", "ERROR"},
		{"healthz ok is DEBUG", 200, "/healthz", "DEBUG"},
		{"readyz ok is DEBUG", 200, "/readyz", "DEBUG"},
		{"503 is ERROR", 503, "/readyz", "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := accessLog(t, time.Hour, tt.status, "GET", tt.target, 0)
			if rec["level"] != tt.wantLevel {
				t.Fatalf("level = %v, want %s", rec["level"], tt.wantLevel)
			}
			if rec["msg"] != "request completed" || rec["method"] != "GET" || rec["path"] != strings.SplitN(tt.target, "?", 2)[0] {
				t.Fatalf("record = %v", rec)
			}
			if int(rec["status"].(float64)) != tt.status || int(rec["bytes"].(float64)) != 5 {
				t.Fatalf("status/bytes wrong: %v", rec)
			}
			if rec["request_id"] != "gen-1" || rec["actor"] != "alice" {
				t.Fatalf("missing correlation fields: %v", rec)
			}
			if _, ok := rec["duration_ms"].(float64); !ok {
				t.Fatalf("duration_ms missing: %v", rec)
			}
			if _, ok := rec["remote_addr"]; !ok {
				t.Fatalf("remote_addr missing: %v", rec)
			}
		})
	}
}

func TestSlowRequestIsWarned(t *testing.T) {
	rec := accessLog(t, 20*time.Millisecond, 200, "GET", "/v1/flags", 40*time.Millisecond)
	if rec["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN for a slow request", rec["level"])
	}
	if rec := accessLog(t, 0, 200, "GET", "/v1/flags", 5*time.Millisecond); rec["level"] != "INFO" {
		t.Fatalf("slow threshold of 0 must disable the warning, got %v", rec["level"])
	}
}

func TestAccessLogIncludesUserIDButNeverTheQueryString(t *testing.T) {
	rec := accessLog(t, time.Hour, 200, "GET", "/v1/evaluate?flag=f&user_id=u-9&secret=hunter2", 0)
	if rec["user_id"] != "u-9" {
		t.Fatalf("user_id = %v", rec["user_id"])
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "secret") {
		t.Fatalf("query string leaked into the log: %s", raw)
	}
	if rec["path"] != "/v1/evaluate" {
		t.Fatalf("path = %v", rec["path"])
	}
	if _, has := accessLog(t, time.Hour, 200, "GET", "/v1/flags", 0)["user_id"]; has {
		t.Fatal("user_id logged for a request without one")
	}
}

func TestRecoverTurnsPanicIntoLogged500(t *testing.T) {
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom secret-internal-detail")
	}), middleware.RequestID(base, &seqIDs{}), middleware.Actor(base), middleware.Logging(time.Hour), middleware.Recover())

	rec := do(h, "GET", "/v1/flags", map[string]string{middleware.ActorHeader: "alice"})

	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "internal" || body.Error.RequestID != "gen-1" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "kaboom") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("panic details leaked to the client: %s", rec.Body.String())
	}

	recs := logs.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want panic + access line: %v", len(recs), recs)
	}
	panicRec, access := recs[0], recs[1]
	if panicRec["level"] != "ERROR" || panicRec["msg"] != "panic recovered" ||
		!strings.Contains(fmt.Sprint(panicRec["panic"]), "kaboom") ||
		!strings.Contains(fmt.Sprint(panicRec["stack"]), "recover.go") ||
		panicRec["request_id"] != "gen-1" || panicRec["actor"] != "alice" {
		t.Fatalf("panic record = %v", panicRec)
	}
	if access["level"] != "ERROR" || int(access["status"].(float64)) != 500 || access["request_id"] != "gen-1" {
		t.Fatalf("a panicking request must still produce a 500 access line: %v", access)
	}
}

func TestRecoverDoesNotCorruptAnAlreadyStartedResponse(t *testing.T) {
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "partial")
		panic("late failure")
	}), middleware.RequestID(base, &seqIDs{}), middleware.Recover())

	rec := do(h, "GET", "/", nil)
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Fatalf("got %d %q: recovery overwrote a started response", rec.Code, rec.Body.String())
	}
	if len(logs.records(t)) != 1 {
		t.Fatal("the panic must still be logged")
	}
}

func TestRecoverRepanicsErrAbortHandler(t *testing.T) {
	h := middleware.Recover()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to propagate", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	t.Fatal("expected a panic")
}

func TestRecoverPassesThroughNormalRequests(t *testing.T) {
	h := middleware.Recover()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	if rec := do(h, "GET", "/", nil); rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.BodyLimit(10)(read)

	send := func(body string, declareLength bool) int {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if !declareLength {
			r.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if got := send("0123456789", true); got != 200 {
		t.Errorf("body at the limit: %d", got)
	}
	if got := send("01234567890", true); got != 413 {
		t.Errorf("declared length over the limit: %d, want 413", got)
	}
	if got := send(strings.Repeat("x", 1000), false); got != 413 {
		t.Errorf("undeclared length over the limit: %d, want the read to be cut off", got)
	}

	t.Run("early rejection does not run the handler and uses the error shape", func(t *testing.T) {
		called := false
		h := middleware.BodyLimit(5)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("too long body")))
		var body httpx.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "body_too_large" {
			t.Fatalf("body = %q (%v)", rec.Body.String(), err)
		}
		if called || rec.Code != 413 {
			t.Fatalf("called=%v status=%d", called, rec.Code)
		}
	})
}

func TestStatusWriterKeepsFirstStatusAndUnwraps(t *testing.T) {
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusInternalServerError) // net/http ignores this; so must the log
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush through the wrapper failed: %v", err)
		}
	}), middleware.RequestID(base, &seqIDs{}), middleware.Logging(time.Hour))
	do(h, "POST", "/x", nil)

	if got := int(logs.records(t)[0]["status"].(float64)); got != 201 {
		t.Fatalf("logged status = %d, want the first status 201", got)
	}
}

func TestLoggingIsSafeUnderConcurrency(t *testing.T) {
	base, logs := newLog()
	h := middleware.Chain(http.HandlerFunc(ok),
		middleware.RequestID(base, &seqIDs{}), middleware.Actor(base), middleware.Logging(time.Hour), middleware.Recover())

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				do(h, "GET", "/v1/flags", map[string]string{middleware.ActorHeader: fmt.Sprintf("u%d", g)})
			}
		}(g)
	}
	wg.Wait()

	recs := logs.records(t)
	if len(recs) != 1600 {
		t.Fatalf("got %d access lines, want 1600", len(recs))
	}
	seen := map[string]bool{}
	for _, r := range recs {
		id := r["request_id"].(string)
		if seen[id] {
			t.Fatalf("request id %s appears twice", id)
		}
		seen[id] = true
	}
}
