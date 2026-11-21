package router_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"feature-flag-service/internal/dao/memory"
	"feature-flag-service/internal/evaluator"
	"feature-flag-service/internal/handler"
	"feature-flag-service/internal/router"
	"feature-flag-service/internal/service"
	"feature-flag-service/pkg/cache/lfu"
	"feature-flag-service/pkg/httpx"
)

// newServer wires the real services, in-memory DAOs and the real LFU cache
// behind the real router, the same way the composition root does.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	flagDAO, auditDAO := memory.NewFlagDAO(), memory.NewAuditDAO()
	cache, err := lfu.New(lfu.Config{Capacity: 1000, DefaultTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })

	ev, err := evaluator.NewDefault(flagDAO)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := service.NewFlagService(flagDAO, auditDAO, cache)
	if err != nil {
		t.Fatal(err)
	}
	evals, err := service.NewEvaluationService(ev, cache, 0)
	if err != nil {
		t.Fatal(err)
	}
	audits, err := service.NewAuditService(auditDAO)
	if err != nil {
		t.Fatal(err)
	}

	root, err := router.New(router.Deps{
		Health:    handler.NewHealthHandler(alwaysReady{}),
		Flags:     handler.NewFlagHandler(flags),
		Overrides: handler.NewOverrideHandler(flags),
		Evaluate:  handler.NewEvaluateHandler(evals),
		Audit:     handler.NewAuditHandler(audits),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		IDs:       service.RandomIDGenerator{},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return srv
}

type alwaysReady struct{}

func (alwaysReady) Ready() bool { return true }

type client struct {
	t    *testing.T
	base string
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (c client) do(actor, method, path, body string, headers ...string) response {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
}

func (c client) evaluate(flag, user string) handler.EvaluateResponse {
	c.t.Helper()
	r := c.do("", "GET", "/v1/evaluate?flag="+flag+"&user_id="+user, "")
	if r.status != 200 {
		c.t.Fatalf("evaluate %s/%s: %d %s", flag, user, r.status, r.body)
	}
	var out handler.EvaluateResponse
	r.json(c.t, &out)
	return out
}

func TestFlagLifecycleThroughHTTP(t *testing.T) {
	c := client{t, newServer(t).URL}

	r := c.do("alice", "POST", "/v1/flags", `{"name":"new-checkout","description":"New checkout","enabled":false}`,
		"X-Request-ID", "req-create")
	if r.status != 201 || r.header.Get("Location") != "/v1/flags/new-checkout" {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	if r.header.Get("X-Request-ID") != "req-create" {
		t.Fatalf("request id not echoed: %q", r.header.Get("X-Request-ID"))
	}

	if got := c.evaluate("new-checkout", "u1"); got.Enabled || got.Reason != "global_off" || got.Source != "store" {
		t.Fatalf("first evaluation: %+v", got)
	}
	if got := c.evaluate("new-checkout", "u1"); got.Source != "cache" || got.Enabled {
		t.Fatalf("second evaluation should come from the cache: %+v", got)
	}

	// An override takes effect on the very next evaluation, and only for its user.
	if r := c.do("alice", "PUT", "/v1/flags/new-checkout/overrides/u1", `{"enabled":true}`); r.status != 200 {
		t.Fatalf("set override: %d %s", r.status, r.body)
	}
	if got := c.evaluate("new-checkout", "u1"); !got.Enabled || got.Reason != "user_override" || got.Source != "store" {
		t.Fatalf("override not visible immediately: %+v", got)
	}
	if got := c.evaluate("new-checkout", "u2"); got.Enabled || got.Reason != "global_off" {
		t.Fatalf("override leaked to another user: %+v", got)
	}

	// Turning the flag on globally reaches users with no override at once.
	c.evaluate("new-checkout", "u2")
	if r := c.do("bob", "PUT", "/v1/flags/new-checkout", `{"enabled":true}`, "X-Request-ID", "req-enable"); r.status != 200 {
		t.Fatalf("update: %d %s", r.status, r.body)
	}
	if got := c.evaluate("new-checkout", "u2"); !got.Enabled || got.Reason != "global_on" || got.Source != "store" {
		t.Fatalf("global change not visible immediately (stale cache?): %+v", got)
	}

	// Removing the override falls back to the global state.
	if r := c.do("carol", "DELETE", "/v1/flags/new-checkout/overrides/u1", ""); r.status != 204 {
		t.Fatalf("remove override: %d", r.status)
	}
	c.do("alice", "PUT", "/v1/flags/new-checkout/overrides/vip", `{"enabled":false}`)
	if got := c.evaluate("new-checkout", "vip"); got.Enabled || got.Reason != "user_override" {
		t.Fatalf("explicit off override must beat global on: %+v", got)
	}

	var list handler.FlagListResponse
	c.do("", "GET", "/v1/flags", "").json(t, &list)
	if len(list.Flags) != 1 || list.Flags[0].Version != 5 || list.Flags[0].Overrides["vip"] != false {
		t.Fatalf("list: %+v", list)
	}

	if r := c.do("dave", "DELETE", "/v1/flags/new-checkout", "", "X-Request-ID", "req-delete"); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	if r := c.do("", "GET", "/v1/flags/new-checkout", ""); r.status != 404 {
		t.Fatalf("get after delete: %d", r.status)
	}
	if r := c.do("", "GET", "/v1/evaluate?flag=new-checkout&user_id=u1", ""); r.status != 404 {
		t.Fatalf("evaluate after delete: %d (a cached result survived the delete)", r.status)
	}

	// The audit trail outlives the flag, newest first, and ties back to the request ids.
	var audit handler.AuditResponse
	if r := c.do("", "GET", "/v1/flags/new-checkout/audit?limit=100", ""); r.status != 200 {
		t.Fatalf("audit: %d %s", r.status, r.body)
	} else {
		r.json(t, &audit)
	}
	var actions, actors []string
	for _, e := range audit.Entries {
		actions = append(actions, e.Action)
		actors = append(actors, e.Actor)
	}
	wantActions := "flag.deleted override.set override.removed flag.updated override.set flag.created"
	wantActors := "dave alice carol bob alice alice"
	if strings.Join(actions, " ") != wantActions || strings.Join(actors, " ") != wantActors {
		t.Fatalf("audit order:\n actions %v\n actors  %v", actions, actors)
	}
	if audit.Entries[0].RequestID != "req-delete" || audit.Entries[3].RequestID != "req-enable" || audit.Entries[5].RequestID != "req-create" {
		t.Fatalf("audit request ids do not match the HTTP requests: %v / %v / %v",
			audit.Entries[0].RequestID, audit.Entries[3].RequestID, audit.Entries[5].RequestID)
	}

	var before, after struct {
		Enabled bool  `json:"enabled"`
		Version int64 `json:"version"`
	}
	json.Unmarshal(audit.Entries[3].Before, &before)
	json.Unmarshal(audit.Entries[3].After, &after)
	if before.Enabled || !after.Enabled || after.Version != before.Version+1 {
		t.Fatalf("update audit before/after wrong: %s -> %s", audit.Entries[3].Before, audit.Entries[3].After)
	}
	if string(audit.Entries[5].Before) != "null" || string(audit.Entries[0].After) != "null" {
		t.Fatalf("create must have before=null and delete after=null: %s / %s", audit.Entries[5].Before, audit.Entries[0].After)
	}
}

// TestDocumentedStatusCodes checks every endpoint against the status codes
// promised in PLAN.md section 5.
func TestDocumentedStatusCodes(t *testing.T) {
	c := client{t, newServer(t).URL}
	if r := c.do("a", "POST", "/v1/flags", `{"name":"f","enabled":true}`); r.status != 201 {
		t.Fatal(r.status)
	}
	c.do("a", "PUT", "/v1/flags/f/overrides/u", `{"enabled":false}`)

	tests := []struct {
		name                string
		actor, method, path string
		body                string
		want                int
		wantCode            string
	}{
		{"create ok", "a", "POST", "/v1/flags", `{"name":"g"}`, 201, ""},
		{"create invalid name", "a", "POST", "/v1/flags", `{"name":"has space"}`, 400, "invalid_request"},
		{"create name too long", "a", "POST", "/v1/flags", `{"name":"` + strings.Repeat("x", 65) + `"}`, 400, "invalid_request"},
		{"create duplicate", "a", "POST", "/v1/flags", `{"name":"f"}`, 409, "already_exists"},
		{"create no actor", "", "POST", "/v1/flags", `{"name":"h"}`, 400, "missing_actor"},
		{"list", "", "GET", "/v1/flags", "", 200, ""},
		{"get ok", "", "GET", "/v1/flags/f", "", 200, ""},
		{"get missing", "", "GET", "/v1/flags/ghost", "", 404, "not_found"},
		{"get invalid name", "", "GET", "/v1/flags/bad%20name", "", 400, "invalid_request"},
		{"update ok", "a", "PUT", "/v1/flags/f", `{"description":"new"}`, 200, ""},
		{"update nothing", "a", "PUT", "/v1/flags/f", `{}`, 400, "invalid_request"},
		{"update missing", "a", "PUT", "/v1/flags/ghost", `{"enabled":true}`, 404, "not_found"},
		{"update bad body", "a", "PUT", "/v1/flags/f", `{"enabled":"yes"}`, 400, "invalid_request"},
		{"set override ok", "a", "PUT", "/v1/flags/f/overrides/u2", `{"enabled":true}`, 200, ""},
		{"set override missing flag", "a", "PUT", "/v1/flags/ghost/overrides/u", `{"enabled":true}`, 404, "not_found"},
		{"set override no value", "a", "PUT", "/v1/flags/f/overrides/u", `{}`, 400, "invalid_request"},
		{"remove override ok", "a", "DELETE", "/v1/flags/f/overrides/u", "", 204, ""},
		{"remove override again", "a", "DELETE", "/v1/flags/f/overrides/u", "", 404, "not_found"},
		{"remove override missing flag", "a", "DELETE", "/v1/flags/ghost/overrides/u", "", 404, "not_found"},
		{"evaluate ok", "", "GET", "/v1/evaluate?flag=f&user_id=u", "", 200, ""},
		{"evaluate missing flag param", "", "GET", "/v1/evaluate?user_id=u", "", 400, "invalid_request"},
		{"evaluate missing user param", "", "GET", "/v1/evaluate?flag=f", "", 400, "invalid_request"},
		{"evaluate unknown flag", "", "GET", "/v1/evaluate?flag=ghost&user_id=u", "", 404, "not_found"},
		{"evaluate invalid flag name", "", "GET", "/v1/evaluate?flag=bad+name&user_id=u", "", 400, "invalid_request"},
		{"audit ok", "", "GET", "/v1/flags/f/audit", "", 200, ""},
		{"audit bad limit", "", "GET", "/v1/flags/f/audit?limit=x", "", 400, "invalid_request"},
		{"audit negative limit", "", "GET", "/v1/flags/f/audit?limit=-3", "", 400, "invalid_request"},
		{"audit unknown flag is empty not 404", "", "GET", "/v1/flags/ghost/audit", "", 200, ""},
		{"delete ok", "a", "DELETE", "/v1/flags/g", "", 204, ""},
		{"delete missing", "a", "DELETE", "/v1/flags/g", "", 404, "not_found"},
		{"healthz", "", "GET", "/healthz", "", 200, ""},
		{"readyz", "", "GET", "/readyz", "", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := c.do(tt.actor, tt.method, tt.path, tt.body)
			if r.status != tt.want {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.want, r.body)
			}
			if tt.wantCode != "" {
				var body httpx.ErrorBody
				r.json(t, &body)
				if body.Error.Code != tt.wantCode || body.Error.RequestID == "" {
					t.Fatalf("error body = %+v", body)
				}
			}
			if r.status >= 400 && r.status < 500 && r.header.Get("Content-Type") != "application/json" {
				t.Fatalf("error content type = %q", r.header.Get("Content-Type"))
			}
		})
	}
}

func TestNoMissingFlagEverProducesA500(t *testing.T) {
	c := client{t, newServer(t).URL}
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/v1/flags/ghost", ""},
		{"PUT", "/v1/flags/ghost", `{"enabled":true}`},
		{"DELETE", "/v1/flags/ghost", ""},
		{"PUT", "/v1/flags/ghost/overrides/u", `{"enabled":true}`},
		{"DELETE", "/v1/flags/ghost/overrides/u", ""},
		{"GET", "/v1/evaluate?flag=ghost&user_id=u", ""},
	} {
		if r := c.do("a", rt.method, rt.path, rt.body); r.status != 404 {
			t.Errorf("%s %s = %d, want 404", rt.method, rt.path, r.status)
		}
	}
}

func TestOnlyOneOfManyConcurrentCreatesWins(t *testing.T) {
	c := client{t, newServer(t).URL}
	const n = 30
	statuses := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses <- c.do(fmt.Sprintf("actor%d", i), "POST", "/v1/flags", `{"name":"contested"}`).status
		}(i)
	}
	wg.Wait()
	close(statuses)

	counts := map[int]int{}
	for s := range statuses {
		counts[s]++
	}
	if counts[201] != 1 || counts[409] != n-1 {
		t.Fatalf("statuses = %v, want exactly one 201 and %d 409s", counts, n-1)
	}
	var audit handler.AuditResponse
	c.do("", "GET", "/v1/flags/contested/audit", "").json(t, &audit)
	if len(audit.Entries) != 1 {
		t.Fatalf("%d audit entries for a flag created once", len(audit.Entries))
	}
}

// After a write returns to its client, every later evaluation must see it,
// even while many other clients are evaluating the same flag.
func TestReadYourWritesOverHTTPUnderLoad(t *testing.T) {
	c := client{t, newServer(t).URL}
	c.do("admin", "POST", "/v1/flags", `{"name":"f","enabled":true}`)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r := c.do("", "GET", fmt.Sprintf("/v1/evaluate?flag=f&user_id=u%d", g%3), "")
					if r.status != 200 {
						t.Errorf("reader got %d: %s", r.status, r.body)
						return
					}
				}
			}
		}(g)
	}

	for i := 0; i < 80; i++ {
		want := i%2 == 0
		r := c.do("admin", "PUT", "/v1/flags/f", fmt.Sprintf(`{"enabled":%v}`, want))
		if r.status != 200 {
			t.Fatalf("update %d: %d %s", i, r.status, r.body)
		}
		for u := 0; u < 3; u++ {
			if got := c.evaluate("f", fmt.Sprintf("u%d", u)); got.Enabled != want {
				close(stop)
				wg.Wait()
				t.Fatalf("iteration %d: user u%d saw enabled=%v from %s after the flag was set to %v", i, u, got.Enabled, got.Source, want)
			}
		}
	}
	close(stop)
	wg.Wait()
}
