package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"feature-flag-service/internal/domain"
	"feature-flag-service/internal/evaluator"
	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/router"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/httpx"
)

// ---- fakes: one per handler-side interface --------------------------------

type fakeFlags struct {
	err   error
	flag  domain.Flag
	list  []domain.Flag
	calls []string

	name, desc, user string
	enabled          bool
	update           service.FlagUpdate

	createdRollout int // rollout the Create call ended up with (100 when no option)
	createOptions  int // how many options Create received
}

func (f *fakeFlags) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeFlags) Create(_ context.Context, name, desc string, enabled bool, opts ...domain.FlagOption) (domain.Flag, error) {
	f.record("Create")
	f.name, f.desc, f.enabled = name, desc, enabled

	// Apply the options the way the real service does, so a test can see which
	// rollout reached it and an invalid one is rejected before "storing".
	applied := domain.Flag{Name: name}
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			return domain.Flag{}, err
		}
	}
	f.createdRollout, f.createOptions = applied.Rollout(), len(opts)
	return f.flag, f.err
}
func (f *fakeFlags) Get(_ context.Context, name string) (domain.Flag, error) {
	f.record("Get")
	f.name = name
	return f.flag, f.err
}
func (f *fakeFlags) List(context.Context) ([]domain.Flag, error) {
	f.record("List")
	return f.list, f.err
}
func (f *fakeFlags) Update(_ context.Context, name string, u service.FlagUpdate) (domain.Flag, error) {
	f.record("Update")
	f.name, f.update = name, u
	return f.flag, f.err
}
func (f *fakeFlags) Delete(_ context.Context, name string) error {
	f.record("Delete")
	f.name = name
	return f.err
}
func (f *fakeFlags) SetOverride(_ context.Context, name, user string, enabled bool) (domain.Flag, error) {
	f.record("SetOverride")
	f.name, f.user, f.enabled = name, user, enabled
	return f.flag, f.err
}
func (f *fakeFlags) RemoveOverride(_ context.Context, name, user string) (domain.Flag, error) {
	f.record("RemoveOverride")
	f.name, f.user = name, user
	return f.flag, f.err
}

type fakeEval struct {
	err        error
	result     service.Evaluation
	calls      int
	flag, user string
}

func (f *fakeEval) Evaluate(_ context.Context, flag, user string) (service.Evaluation, error) {
	f.calls++
	f.flag, f.user = flag, user
	return f.result, f.err
}

type fakeAudit struct {
	err     error
	entries []domain.AuditEntry
	calls   int
	name    string
	limit   int
}

func (f *fakeAudit) History(_ context.Context, name string, limit int) ([]domain.AuditEntry, error) {
	f.calls++
	f.name, f.limit = name, limit
	return f.entries, f.err
}

type fakeReady struct{ ready atomic.Bool }

func (f *fakeReady) Ready() bool { return f.ready.Load() }

type seqIDs struct{ n atomic.Int64 }

func (s *seqIDs) NewID() string { return fmt.Sprintf("rid-%d", s.n.Add(1)) }

type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	h     http.Handler
	flags *fakeFlags
	eval  *fakeEval
	audit *fakeAudit
	ready *fakeReady
	logs  *logBuf
}

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func sampleFlag() domain.Flag {
	return domain.Flag{
		Name: "new-checkout", Description: "d", Enabled: true,
		Overrides: map[string]bool{"u1": false},
		Version:   3, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour),
	}
}

func newHarness(t *testing.T, mutate ...func(*router.Deps)) *harness {
	t.Helper()
	h := &harness{
		flags: &fakeFlags{flag: sampleFlag()},
		eval:  &fakeEval{},
		audit: &fakeAudit{},
		ready: &fakeReady{},
		logs:  &logBuf{},
	}
	h.ready.ready.Store(true)
	deps := router.Deps{
		Health:    handler.NewHealthHandler(h.ready),
		Flags:     handler.NewFlagHandler(h.flags),
		Overrides: handler.NewOverrideHandler(h.flags),
		Evaluate:  handler.NewEvaluateHandler(h.eval),
		Audit:     handler.NewAuditHandler(h.audit),
		Logger:    slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		IDs:       &seqIDs{},
	}
	for _, m := range mutate {
		m(&deps)
	}
	root, err := router.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	h.h = root
	return h
}

// do sends a request with a default actor and JSON content type. An extra
// header with an empty value removes that default.
func (h *harness) do(method, target, body string, extra ...string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Actor", "alice")
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "" {
			r.Header.Del(extra[i])
		} else {
			r.Header.Set(extra[i], extra[i+1])
		}
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorDetail {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code == "" {
		t.Fatalf("not an error body (%v): %s", err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error content type = %q", ct)
	}
	return body.Error
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

// ---- error mapping ---------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid", domain.Invalidf("bad %s", "thing"), 400, "invalid_request"},
		{"not found", domain.NotFoundf("flag %q", "x"), 404, "not_found"},
		{"already exists", domain.AlreadyExistsf("flag %q", "x"), 409, "already_exists"},
		{"conflict", domain.Conflictf("changed"), 409, "conflict"},
		{"audit failed", fmt.Errorf("%w: flag: %w", service.ErrAuditFailed, errors.New("disk full /var/secret/path")), 500, "audit_failed"},
		{"deadline", context.DeadlineExceeded, 504, "timeout"},
		{"cancelled", context.Canceled, 499, "client_closed_request"},
		{"wrapped not found", fmt.Errorf("layer: %w", fmt.Errorf("inner: %w", domain.ErrNotFound)), 404, "not_found"},
		{"evaluator misconfiguration", evaluator.ErrNoDecision, 500, "internal"},
		{"unknown error", errors.New("pq: password authentication failed for user admin"), 500, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.flags.err = tt.err
			rec := h.do("GET", "/v1/flags/x", "")

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			e := errorOf(t, rec)
			if e.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", e.Code, tt.wantCode)
			}
			if e.RequestID != "rid-1" || rec.Header().Get("X-Request-ID") != "rid-1" {
				t.Fatalf("request id missing from error response: %+v", e)
			}
			if tt.wantStatus >= 500 {
				for _, secret := range []string{"password", "admin", "/var/secret", "disk full", "pq:"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Fatalf("internal detail %q leaked to the client: %s", secret, rec.Body.String())
					}
				}
			}
		})
	}
}

func TestUnexpectedErrorCauseIsLoggedWithRequestID(t *testing.T) {
	h := newHarness(t)
	h.flags.err = errors.New("pq: password authentication failed")
	h.do("GET", "/v1/flags/x", "", "X-Request-ID", "trace-9")

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["msg"] == "request failed" {
			found = true
			if rec["level"] != "ERROR" || rec["request_id"] != "trace-9" ||
				!strings.Contains(fmt.Sprint(rec["error"]), "password authentication failed") {
				t.Fatalf("record = %v", rec)
			}
		}
	}
	if !found {
		t.Fatalf("the real cause was not logged:\n%s", h.logs.String())
	}
}

func TestClientErrorsDoNotLogAnErrorLine(t *testing.T) {
	h := newHarness(t)
	h.flags.err = domain.NotFoundf("flag %q", "x")
	h.do("GET", "/v1/flags/x", "")
	if strings.Contains(h.logs.String(), `"msg":"request failed"`) {
		t.Fatal("a 404 produced an ERROR 'request failed' line")
	}
}

// ---- flags -----------------------------------------------------------------

func TestCreateFlag(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/v1/flags", `{"name":"new-checkout","description":"d","enabled":true}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/flags/new-checkout" {
		t.Fatalf("Location = %q", loc)
	}
	if h.flags.name != "new-checkout" || h.flags.desc != "d" || !h.flags.enabled {
		t.Fatalf("service got %q %q %v", h.flags.name, h.flags.desc, h.flags.enabled)
	}
	got := decode[handler.FlagResponse](t, rec)
	want := handler.FlagResponse{
		Name: "new-checkout", Description: "d", Enabled: true, Overrides: map[string]bool{"u1": false},
		RolloutPercent: 100, Version: 3, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestCreateDefaultsToDisabled(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/v1/flags", `{"name":"f"}`)
	if h.flags.enabled {
		t.Fatal("a flag created without 'enabled' must start disabled")
	}
}

func TestCreateLocationIsEscaped(t *testing.T) {
	h := newHarness(t)
	h.flags.flag.Name = "a.b-c_d"
	rec := h.do("POST", "/v1/flags", `{"name":"a.b-c_d"}`)
	if loc := rec.Header().Get("Location"); loc != "/v1/flags/a.b-c_d" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestRequestBodyProblemsNeverReachTheService(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"empty body", "", "", 400, "invalid_request"},
		{"malformed json", `{"name":`, "application/json", 400, "invalid_request"},
		{"unknown field", `{"name":"f","enabeld":true}`, "application/json", 400, "invalid_request"},
		{"wrong type", `{"name":"f","enabled":"yes"}`, "application/json", 400, "invalid_request"},
		{"form encoded", `name=f`, "application/x-www-form-urlencoded", 415, "unsupported_media_type"},
		{"too large", `{"name":"` + strings.Repeat("a", 2<<20) + `"}`, "application/json", 413, "body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			r := httptest.NewRequest("POST", "/v1/flags", strings.NewReader(tt.body))
			r.Header.Set("X-Actor", "alice")
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()
			h.h.ServeHTTP(rec, r)

			if rec.Code != tt.wantStatus || errorOf(t, rec).Code != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tt.wantStatus, tt.wantCode)
			}
			if len(h.flags.calls) != 0 {
				t.Fatalf("service was called: %v", h.flags.calls)
			}
		})
	}
}

func TestBodyOverTheConfiguredLimitIsRejectedWithoutChunking(t *testing.T) {
	h := newHarness(t, func(d *router.Deps) { d.MaxBodyBytes = 32 })
	r := httptest.NewRequest("POST", "/v1/flags", strings.NewReader(`{"name":"`+strings.Repeat("a", 100)+`"}`))
	r.Header.Set("X-Actor", "alice")
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = -1 // chunked: the declared length cannot short-circuit
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	if rec.Code != 413 || errorOf(t, rec).Code != "body_too_large" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestListFlags(t *testing.T) {
	h := newHarness(t)
	h.flags.list = []domain.Flag{{Name: "a", Version: 1}, sampleFlag()}
	rec := h.do("GET", "/v1/flags", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode[handler.FlagListResponse](t, rec)
	if len(got.Flags) != 2 || got.Flags[0].Name != "a" || got.Flags[1].Overrides["u1"] != false {
		t.Fatalf("body = %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"overrides":{}`) {
		t.Fatalf("a flag with no overrides must render {} not null: %s", rec.Body.String())
	}
}

func TestListWithNoFlagsIsAnEmptyArrayNotNull(t *testing.T) {
	h := newHarness(t)
	h.flags.list = nil
	rec := h.do("GET", "/v1/flags", "")
	if strings.TrimSpace(rec.Body.String()) != `{"flags":[]}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestGetFlag(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/flags/new-checkout", "")
	if rec.Code != 200 || h.flags.name != "new-checkout" {
		t.Fatalf("status %d, name %q", rec.Code, h.flags.name)
	}
	if got := decode[handler.FlagResponse](t, rec); got.Name != "new-checkout" || got.Version != 3 {
		t.Fatalf("body = %+v", got)
	}
}

func TestUpdateFlagPassesOnlyProvidedFields(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantDesc *string
		wantEn   *bool
	}{
		{"both", `{"description":"x","enabled":false}`, ptr("x"), ptr(false)},
		{"only enabled", `{"enabled":true}`, nil, ptr(true)},
		{"only description", `{"description":""}`, ptr(""), nil},
		{"nothing", `{}`, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("PUT", "/v1/flags/new-checkout", tt.body)
			if rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			u := h.flags.update
			if (u.Description == nil) != (tt.wantDesc == nil) || (u.Description != nil && *u.Description != *tt.wantDesc) {
				t.Fatalf("description = %v, want %v", u.Description, tt.wantDesc)
			}
			if (u.Enabled == nil) != (tt.wantEn == nil) || (u.Enabled != nil && *u.Enabled != *tt.wantEn) {
				t.Fatalf("enabled = %v, want %v", u.Enabled, tt.wantEn)
			}
			if h.flags.name != "new-checkout" {
				t.Fatalf("name = %q", h.flags.name)
			}
		})
	}
}

func TestDeleteFlag(t *testing.T) {
	h := newHarness(t)
	rec := h.do("DELETE", "/v1/flags/new-checkout", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || h.flags.name != "new-checkout" {
		t.Fatalf("status %d body %q name %q", rec.Code, rec.Body.String(), h.flags.name)
	}
}

func TestServiceErrorsOnEveryFlagRoute(t *testing.T) {
	routes := []struct{ method, target, body string }{
		{"POST", "/v1/flags", `{"name":"f"}`},
		{"GET", "/v1/flags", ""},
		{"GET", "/v1/flags/f", ""},
		{"PUT", "/v1/flags/f", `{"enabled":true}`},
		{"DELETE", "/v1/flags/f", ""},
		{"PUT", "/v1/flags/f/overrides/u", `{"enabled":true}`},
		{"DELETE", "/v1/flags/f/overrides/u", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			h := newHarness(t)
			h.flags.err = domain.NotFoundf("flag %q", "f")
			rec := h.do(rt.method, rt.target, rt.body)
			if rec.Code != 404 || errorOf(t, rec).Code != "not_found" {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---- overrides -------------------------------------------------------------

func TestSetOverride(t *testing.T) {
	h := newHarness(t)
	rec := h.do("PUT", "/v1/flags/new-checkout/overrides/u1", `{"enabled":false}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.flags.name != "new-checkout" || h.flags.user != "u1" || h.flags.enabled {
		t.Fatalf("service got %q %q %v", h.flags.name, h.flags.user, h.flags.enabled)
	}
	if got := decode[handler.FlagResponse](t, rec); got.Name != "new-checkout" {
		t.Fatalf("body = %+v", got)
	}
}

func TestSetOverrideDecodesEscapedUserID(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/v1/flags/f/overrides/user%40example.com", `{"enabled":true}`)
	if h.flags.user != "user@example.com" {
		t.Fatalf("user = %q", h.flags.user)
	}
}

func TestSetOverrideRequiresEnabled(t *testing.T) {
	for name, body := range map[string]string{"empty object": `{}`, "null": `{"enabled":null}`} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("PUT", "/v1/flags/f/overrides/u", body)
			if rec.Code != 400 || !strings.Contains(errorOf(t, rec).Message, `"enabled" is required`) {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if len(h.flags.calls) != 0 {
				t.Fatal("service called without a value")
			}
		})
	}
}

func TestRemoveOverride(t *testing.T) {
	h := newHarness(t)
	rec := h.do("DELETE", "/v1/flags/new-checkout/overrides/u1", "")
	if rec.Code != 204 || rec.Body.Len() != 0 || h.flags.name != "new-checkout" || h.flags.user != "u1" {
		t.Fatalf("status %d name %q user %q", rec.Code, h.flags.name, h.flags.user)
	}
}

// ---- evaluate --------------------------------------------------------------

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name       string
		cached     bool
		wantSource string
	}{
		{"served from store", false, "store"},
		{"served from cache", true, "cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.eval.result = service.Evaluation{
				Result: evaluator.Result{Flag: "new-checkout", UserID: "u1", Enabled: true, Reason: evaluator.ReasonUserOverride},
				Cached: tt.cached,
			}
			rec := h.do("GET", "/v1/evaluate?flag=new-checkout&user_id=u1", "")
			if rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			want := handler.EvaluateResponse{Flag: "new-checkout", UserID: "u1", Enabled: true, Reason: "user_override", Source: tt.wantSource}
			if got := decode[handler.EvaluateResponse](t, rec); got != want {
				t.Fatalf("body = %+v, want %+v", got, want)
			}
			if h.eval.flag != "new-checkout" || h.eval.user != "u1" {
				t.Fatalf("service got %q %q", h.eval.flag, h.eval.user)
			}
		})
	}
}

func TestEvaluateRequiresBothParameters(t *testing.T) {
	for _, tt := range []struct{ target, missing string }{
		{"/v1/evaluate", "flag"},
		{"/v1/evaluate?user_id=u", "flag"},
		{"/v1/evaluate?flag=f", "user_id"},
		{"/v1/evaluate?flag=f&user_id=", "user_id"},
		{"/v1/evaluate?flag=&user_id=u", "flag"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("GET", tt.target, "")
			if rec.Code != 400 || !strings.Contains(errorOf(t, rec).Message, tt.missing) {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if h.eval.calls != 0 {
				t.Fatal("evaluator called with missing input")
			}
		})
	}
}

func TestEvaluateErrors(t *testing.T) {
	h := newHarness(t)
	h.eval.err = domain.NotFoundf("flag %q", "ghost")
	rec := h.do("GET", "/v1/evaluate?flag=ghost&user_id=u", "")
	if rec.Code != 404 || errorOf(t, rec).Code != "not_found" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}

	h.eval.err = domain.Invalidf("flag name may not contain spaces")
	rec = h.do("GET", "/v1/evaluate?flag=bad+name&user_id=u", "")
	if rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestEvaluateNeedsNoActor(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1/evaluate?flag=f&user_id=u", "", "X-Actor", "")
	if rec.Code != 200 {
		t.Fatalf("a read without X-Actor got %d", rec.Code)
	}
}

// ---- audit -----------------------------------------------------------------

func TestAuditHistory(t *testing.T) {
	h := newHarness(t)
	h.audit.entries = []domain.AuditEntry{{
		ID: "a1", FlagName: "f", Actor: "bob", Action: domain.ActionFlagUpdated,
		Before: json.RawMessage(`{"enabled":false}`), After: json.RawMessage(`{"enabled":true}`),
		RequestID: "req-1", At: t0,
	}, {
		ID: "a0", FlagName: "f", Actor: "alice", Action: domain.ActionFlagCreated,
		Before: json.RawMessage(`null`), After: json.RawMessage(`{"enabled":false}`), At: t0.Add(-time.Hour),
	}}
	rec := h.do("GET", "/v1/flags/f/audit?limit=2", "")
	if rec.Code != 200 || h.audit.name != "f" || h.audit.limit != 2 {
		t.Fatalf("status %d name %q limit %d", rec.Code, h.audit.name, h.audit.limit)
	}

	got := decode[handler.AuditResponse](t, rec)
	if got.Flag != "f" || len(got.Entries) != 2 {
		t.Fatalf("body = %+v", got)
	}
	e := got.Entries[0]
	if e.ID != "a1" || e.Actor != "bob" || e.Action != "flag.updated" || e.RequestID != "req-1" || !e.At.Equal(t0) {
		t.Fatalf("entry = %+v", e)
	}
	if string(e.Before) != `{"enabled":false}` || string(e.After) != `{"enabled":true}` {
		t.Fatalf("before/after = %s / %s", e.Before, e.After)
	}
	if string(got.Entries[1].Before) != "null" {
		t.Fatalf("a create's before must be null, got %s", got.Entries[1].Before)
	}
}

func TestAuditLimitParsing(t *testing.T) {
	tests := []struct {
		query      string
		wantStatus int
		wantLimit  int
		wantCalled bool
	}{
		{"", 200, 0, true},
		{"?limit=5", 200, 5, true},
		{"?limit=0", 200, 0, true},
		{"?limit=-1", 200, -1, true}, // negative values are the service's decision to reject
		{"?limit=abc", 400, 0, false},
		{"?limit=1.5", 400, 0, false},
		{"?limit=", 200, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("GET", "/v1/flags/f/audit"+tt.query, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if (h.audit.calls == 1) != tt.wantCalled || (tt.wantCalled && h.audit.limit != tt.wantLimit) {
				t.Fatalf("calls=%d limit=%d", h.audit.calls, h.audit.limit)
			}
		})
	}
}

func TestAuditEmptyHistoryIsAnEmptyList(t *testing.T) {
	h := newHarness(t)
	h.audit.entries = nil
	rec := h.do("GET", "/v1/flags/never-existed/audit", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"flag":"never-existed","entries":[]}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuditServiceErrorIsMapped(t *testing.T) {
	h := newHarness(t)
	h.audit.err = domain.Invalidf("limit must not be negative")
	if rec := h.do("GET", "/v1/flags/f/audit?limit=-5", ""); rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---- health ----------------------------------------------------------------

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t)

	rec := h.do("GET", "/healthz", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do("GET", "/readyz", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ready"}` {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body.String())
	}

	h.ready.ready.Store(false)
	rec = h.do("GET", "/readyz", "")
	if rec.Code != 503 || strings.TrimSpace(rec.Body.String()) != `{"status":"shutting_down"}` {
		t.Fatalf("readyz while shutting down: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do("GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("liveness must stay 200 while draining, got %d", rec.Code)
	}
}

// ---- router behaviour ------------------------------------------------------

func TestUnknownRoutesUseTheJSONErrorShape(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{"/", "/nope", "/v1", "/v1/flags/", "/v1/flags/f/unknown", "/v1/flags/f/overrides"} {
		rec := h.do("GET", target, "")
		if rec.Code != 404 || errorOf(t, rec).Code != "not_found" {
			t.Errorf("GET %s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	if len(h.flags.calls) != 0 {
		t.Fatalf("unrouted requests reached a service: %v", h.flags.calls)
	}
}

func TestWrongMethodIs405WithAllowHeader(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		method, target string
		wantAllow      []string
	}{
		{"PATCH", "/v1/flags/f", []string{"GET", "PUT", "DELETE"}},
		{"POST", "/v1/flags/f", []string{"GET", "PUT", "DELETE"}},
		{"DELETE", "/v1/flags", []string{"GET", "POST"}},
		{"POST", "/v1/evaluate", []string{"GET"}},
		{"POST", "/healthz", []string{"GET"}},
		{"GET", "/v1/flags/f/overrides/u", []string{"PUT", "DELETE"}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			rec := h.do(tt.method, tt.target, "")
			if rec.Code != 405 || errorOf(t, rec).Code != "method_not_allowed" {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			allow := rec.Header().Get("Allow")
			for _, m := range tt.wantAllow {
				if !strings.Contains(allow, m) {
					t.Errorf("Allow = %q, missing %s", allow, m)
				}
			}
		})
	}
}

func TestUncleanPathRedirectIsPreserved(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/v1//flags", "")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/v1/flags" {
		t.Fatalf("got %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestWritesRequireAnActor(t *testing.T) {
	writes := []struct{ method, target, body string }{
		{"POST", "/v1/flags", `{"name":"f"}`},
		{"PUT", "/v1/flags/f", `{"enabled":true}`},
		{"DELETE", "/v1/flags/f", ""},
		{"PUT", "/v1/flags/f/overrides/u", `{"enabled":true}`},
		{"DELETE", "/v1/flags/f/overrides/u", ""},
	}
	for _, w := range writes {
		t.Run(w.method+" "+w.target, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do(w.method, w.target, w.body, "X-Actor", "")
			if rec.Code != 400 || errorOf(t, rec).Code != "missing_actor" {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if len(h.flags.calls) != 0 {
				t.Fatalf("a write without an actor reached the service: %v", h.flags.calls)
			}
		})
	}
}

func TestPanicInAHandlerIsAnInternalErrorAndTheServerKeepsServing(t *testing.T) {
	h := newHarness(t, func(d *router.Deps) {
		d.Evaluate = handler.NewEvaluateHandler(panicEvaluator{})
	})

	rec := h.do("GET", "/v1/evaluate?flag=f&user_id=u", "")
	if rec.Code != 500 || errorOf(t, rec).Code != "internal" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatal("panic value leaked")
	}
	if rec := h.do("GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("server not usable after a panic: %d", rec.Code)
	}

	var sawPanic, sawAccess500 bool
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		switch rec["msg"] {
		case "panic recovered":
			sawPanic = rec["level"] == "ERROR" && rec["request_id"] == "rid-1"
		case "request completed":
			if rec["path"] == "/v1/evaluate" {
				sawAccess500 = rec["level"] == "ERROR" && int(rec["status"].(float64)) == 500 && rec["request_id"] == "rid-1"
			}
		}
	}
	if !sawPanic || !sawAccess500 {
		t.Fatalf("a panicking request must log the panic and still log a 500 access line (panic=%v access=%v):\n%s",
			sawPanic, sawAccess500, h.logs.String())
	}
}

type panicEvaluator struct{}

func (panicEvaluator) Evaluate(context.Context, string, string) (service.Evaluation, error) {
	panic("kaboom")
}

func TestRouterRejectsIncompleteDependencies(t *testing.T) {
	good := func() router.Deps {
		h := newHarness(t)
		return router.Deps{
			Health: handler.NewHealthHandler(h.ready), Flags: handler.NewFlagHandler(h.flags),
			Overrides: handler.NewOverrideHandler(h.flags), Evaluate: handler.NewEvaluateHandler(h.eval),
			Audit:  handler.NewAuditHandler(h.audit),
			Logger: slog.New(slog.NewTextHandler(&logBuf{}, nil)), IDs: &seqIDs{},
		}
	}
	if _, err := router.New(good()); err != nil {
		t.Fatalf("complete deps rejected: %v", err)
	}
	mutations := map[string]func(*router.Deps){
		"health":    func(d *router.Deps) { d.Health = nil },
		"flags":     func(d *router.Deps) { d.Flags = nil },
		"overrides": func(d *router.Deps) { d.Overrides = nil },
		"evaluate":  func(d *router.Deps) { d.Evaluate = nil },
		"audit":     func(d *router.Deps) { d.Audit = nil },
		"logger":    func(d *router.Deps) { d.Logger = nil },
		"ids":       func(d *router.Deps) { d.IDs = nil },
	}
	for name, mutate := range mutations {
		d := good()
		mutate(&d)
		if h, err := router.New(d); err == nil || h != nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}

func TestAccessLogCarriesTheWholeTraceForARequest(t *testing.T) {
	h := newHarness(t)
	h.eval.result = service.Evaluation{Result: evaluator.Result{Flag: "f", UserID: "u7", Enabled: true}}
	h.do("GET", "/v1/evaluate?flag=f&user_id=u7", "", "X-Actor", "dana", "X-Request-ID", "trace-77")

	var access map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["msg"] == "request completed" {
			access = rec
		}
	}
	if access == nil {
		t.Fatalf("no access line:\n%s", h.logs.String())
	}
	for k, want := range map[string]any{
		"request_id": "trace-77", "actor": "dana", "method": "GET", "path": "/v1/evaluate",
		"user_id": "u7", "level": "INFO",
	} {
		if access[k] != want {
			t.Errorf("%s = %v, want %v", k, access[k], want)
		}
	}
	if int(access["status"].(float64)) != 200 {
		t.Errorf("status = %v", access["status"])
	}
}

func TestEveryBodyRouteRejectsBadBodies(t *testing.T) {
	routes := []struct{ method, target string }{
		{"POST", "/v1/flags"},
		{"PUT", "/v1/flags/f"},
		{"PUT", "/v1/flags/f/overrides/u"},
	}
	bodies := []struct {
		name, body, contentType string
		wantStatus              int
	}{
		{"empty", "", "", 400},
		{"malformed", `{"enabled":`, "application/json", 400},
		{"unknown field", `{"enabeld":true}`, "application/json", 400},
		{"wrong media type", `enabled=true`, "text/plain", 415},
	}
	for _, rt := range routes {
		for _, b := range bodies {
			t.Run(rt.method+" "+rt.target+" "+b.name, func(t *testing.T) {
				h := newHarness(t)
				r := httptest.NewRequest(rt.method, rt.target, strings.NewReader(b.body))
				r.Header.Set("X-Actor", "alice")
				if b.contentType != "" {
					r.Header.Set("Content-Type", b.contentType)
				}
				rec := httptest.NewRecorder()
				h.h.ServeHTTP(rec, r)
				if rec.Code != b.wantStatus {
					t.Fatalf("status = %d, want %d: %s", rec.Code, b.wantStatus, rec.Body.String())
				}
				errorOf(t, rec)
				if len(h.flags.calls) != 0 {
					t.Fatalf("service called: %v", h.flags.calls)
				}
			})
		}
	}
}

func TestSlowRequestSettingIsHonoured(t *testing.T) {
	slowEval := slowEvaluator{delay: 30 * time.Millisecond}
	level := func(slow time.Duration) string {
		h := newHarness(t, func(d *router.Deps) {
			d.Evaluate = handler.NewEvaluateHandler(slowEval)
			d.SlowRequest = slow
		})
		h.do("GET", "/v1/evaluate?flag=f&user_id=u", "")
		for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
			var rec map[string]any
			json.Unmarshal([]byte(line), &rec)
			if rec["msg"] == "request completed" {
				return rec["level"].(string)
			}
		}
		t.Fatal("no access line")
		return ""
	}
	if got := level(10 * time.Millisecond); got != "WARN" {
		t.Errorf("threshold below the latency: level = %s, want WARN", got)
	}
	if got := level(-1); got != "INFO" {
		t.Errorf("negative threshold must disable the slow warning: level = %s", got)
	}
	if got := level(0); got != "INFO" {
		t.Errorf("zero uses the 1s default, so a 30ms request is INFO: level = %s", got)
	}
}

type slowEvaluator struct{ delay time.Duration }

func (s slowEvaluator) Evaluate(context.Context, string, string) (service.Evaluation, error) {
	time.Sleep(s.delay)
	return service.Evaluation{}, nil
}

// ---- percentage rollout ----------------------------------------------------

func TestCreateFlagPassesTheRolloutToTheService(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int
		wantOpt int
	}{
		{"absent means no option", `{"name":"f"}`, 100, 0},
		{"explicit 30", `{"name":"f","rollout_percent":30}`, 30, 1},
		{"explicit 0 is a real value", `{"name":"f","rollout_percent":0}`, 0, 1},
		{"explicit 100", `{"name":"f","rollout_percent":100}`, 100, 1},
		{"null is the same as absent", `{"name":"f","rollout_percent":null}`, 100, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("POST", "/v1/flags", tt.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if h.flags.createdRollout != tt.want || h.flags.createOptions != tt.wantOpt {
				t.Fatalf("service got rollout %d via %d options, want %d via %d",
					h.flags.createdRollout, h.flags.createOptions, tt.want, tt.wantOpt)
			}
		})
	}
}

func TestCreateFlagWithInvalidRolloutIsABadRequest(t *testing.T) {
	for _, pct := range []string{"-1", "101", "100000"} {
		h := newHarness(t)
		rec := h.do("POST", "/v1/flags", `{"name":"f","rollout_percent":`+pct+`}`)
		if rec.Code != 400 || errorOf(t, rec).Code != "invalid_request" {
			t.Fatalf("rollout %s: got %d %s, want 400 invalid_request", pct, rec.Code, rec.Body.String())
		}
	}
}

func TestRolloutMustBeAWholeNumber(t *testing.T) {
	for _, bad := range []string{`12.5`, `"50"`, `true`, `[50]`, `{"v":50}`} {
		for _, target := range []struct{ method, path, body string }{
			{"POST", "/v1/flags", `{"name":"f","rollout_percent":` + bad + `}`},
			{"PUT", "/v1/flags/f", `{"rollout_percent":` + bad + `}`},
		} {
			h := newHarness(t)
			rec := h.do(target.method, target.path, target.body)
			if rec.Code != 400 || errorOf(t, rec).Code != "invalid_request" {
				t.Fatalf("%s %s: got %d %s, want 400 invalid_request", target.method, target.body, rec.Code, rec.Body.String())
			}
			if len(h.flags.calls) != 0 {
				t.Fatalf("%s %s reached the service: %v", target.method, target.body, h.flags.calls)
			}
		}
	}
}

func TestUpdateFlagPassesTheRolloutThrough(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *int
	}{
		{"set 40", `{"rollout_percent":40}`, ptr(40)},
		{"set 0", `{"rollout_percent":0}`, ptr(0)},
		{"set 100", `{"rollout_percent":100}`, ptr(100)},
		{"absent leaves it alone", `{"enabled":true}`, nil},
		{"null leaves it alone", `{"rollout_percent":null,"enabled":true}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.do("PUT", "/v1/flags/new-checkout", tt.body)
			if rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			got := h.flags.update.RolloutPercent
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("rollout passed on = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateFlagMapsRolloutValidationToBadRequest(t *testing.T) {
	h := newHarness(t)
	h.flags.err = domain.Invalidf("rollout_percent is 101, must be a whole number from 0 to 100")
	rec := h.do("PUT", "/v1/flags/new-checkout", `{"rollout_percent":101}`)
	if rec.Code != 400 || errorOf(t, rec).Code != "invalid_request" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec).Message; !strings.Contains(msg, "rollout_percent") {
		t.Fatalf("the message must name the field, got %q", msg)
	}
}

func TestFlagResponsesAlwaysCarryTheRollout(t *testing.T) {
	restricted := sampleFlag()
	_ = restricted.SetRollout(0)

	tests := []struct {
		name   string
		flag   domain.Flag
		want   int
		method string
		path   string
		body   string
	}{
		{"get unrestricted", sampleFlag(), 100, "GET", "/v1/flags/new-checkout", ""},
		{"get at zero percent", restricted, 0, "GET", "/v1/flags/new-checkout", ""},
		{"create at zero percent", restricted, 0, "POST", "/v1/flags", `{"name":"new-checkout","rollout_percent":0}`},
		{"update", func() domain.Flag { f := sampleFlag(); _ = f.SetRollout(65); return f }(), 65, "PUT", "/v1/flags/new-checkout", `{"rollout_percent":65}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.flags.flag = tt.flag
			rec := h.do(tt.method, tt.path, tt.body)
			var raw map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode: %v: %s", err, rec.Body.String())
			}
			// Zero must not be dropped as "empty": a consumer reading 0% has to
			// see it, not a missing field.
			if v, ok := raw["rollout_percent"]; !ok || v != float64(tt.want) {
				t.Fatalf("rollout_percent = %v (present=%v), want %d in %s", v, ok, tt.want, rec.Body.String())
			}
		})
	}
}

func TestListedFlagsCarryTheirRollout(t *testing.T) {
	h := newHarness(t)
	half := sampleFlag()
	half.Name = "half"
	_ = half.SetRollout(50)
	h.flags.list = []domain.Flag{half, sampleFlag()}

	rec := h.do("GET", "/v1/flags", "")
	var body struct {
		Flags []handler.FlagResponse `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Flags) != 2 {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if body.Flags[0].RolloutPercent != 50 || body.Flags[1].RolloutPercent != 100 {
		t.Fatalf("rollouts = %d, %d; want 50, 100", body.Flags[0].RolloutPercent, body.Flags[1].RolloutPercent)
	}
}

// ---- flag dependencies -----------------------------------------------------

func TestCreateFlagWithRequires(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/v1/flags", `{"name":"f","requires":["b","a"],"rollout_percent":10}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.flags.createOptions != 2 {
		t.Fatalf("service received %d options, want rollout and requires", h.flags.createOptions)
	}

	h = newHarness(t)
	h.do("POST", "/v1/flags", `{"name":"f","requires":[]}`)
	if h.flags.createOptions != 0 {
		t.Fatal("an empty requires list must not become an option")
	}
}

func TestCreateFlagWithBadRequiresIsABadRequest(t *testing.T) {
	for _, body := range []string{
		`{"name":"f","requires":["f"]}`,
		`{"name":"f","requires":["a","a"]}`,
		`{"name":"f","requires":["has space"]}`,
	} {
		h := newHarness(t)
		rec := h.do("POST", "/v1/flags", body)
		if rec.Code != 400 || errorOf(t, rec).Code != "invalid_request" {
			t.Fatalf("%s: got %d %s", body, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{`{"name":"f","requires":"a"}`, `{"name":"f","requires":[1]}`} {
		h := newHarness(t)
		if rec := h.do("POST", "/v1/flags", body); rec.Code != 400 || len(h.flags.calls) != 0 {
			t.Fatalf("%s: got %d, calls %v", body, rec.Code, h.flags.calls)
		}
	}
}

func TestUpdateFlagRequiresSemantics(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantNil bool
		want    []string
	}{
		{"set", `{"requires":["a","b"]}`, false, []string{"a", "b"}},
		{"clear with empty list", `{"requires":[]}`, false, []string{}},
		{"null keeps", `{"requires":null,"enabled":true}`, true, nil},
		{"absent keeps", `{"enabled":true}`, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if rec := h.do("PUT", "/v1/flags/new-checkout", tt.body); rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			got := h.flags.update.Requires
			if (got == nil) != tt.wantNil || (got != nil && fmt.Sprint(*got) != fmt.Sprint(tt.want)) {
				t.Fatalf("service got %v, want nil=%v %v", got, tt.wantNil, tt.want)
			}
		})
	}
}

func TestDependencyErrorsMapToStatusCodes(t *testing.T) {
	h := newHarness(t)
	h.flags.err = domain.Conflictf("requires would create a dependency cycle: a -> b -> a")
	rec := h.do("PUT", "/v1/flags/a", `{"requires":["b"]}`)
	if rec.Code != 409 || errorOf(t, rec).Code != "conflict" {
		t.Fatalf("cycle: got %d %s", rec.Code, rec.Body.String())
	}

	h = newHarness(t)
	h.flags.err = domain.Conflictf(`flag "payments" is required by checkout`)
	if rec := h.do("DELETE", "/v1/flags/payments", ""); rec.Code != 409 {
		t.Fatalf("delete of a required flag: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestResponsesAlwaysCarryARequiresArray(t *testing.T) {
	h := newHarness(t)
	var raw map[string]any
	_ = json.Unmarshal(h.do("GET", "/v1/flags/new-checkout", "").Body.Bytes(), &raw)
	if v, ok := raw["requires"].([]any); !ok || len(v) != 0 {
		t.Fatalf("requires = %#v, want an empty array, not null or missing", raw["requires"])
	}

	h.flags.flag.Requires = []string{"a", "b"}
	raw = nil
	_ = json.Unmarshal(h.do("GET", "/v1/flags/new-checkout", "").Body.Bytes(), &raw)
	if v, _ := raw["requires"].([]any); len(v) != 2 {
		t.Fatalf("requires = %#v", raw["requires"])
	}
}
