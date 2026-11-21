package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"feature-flag-service/internal/app"
	"feature-flag-service/internal/config"
)

// testConfig returns a config using fresh temp directories and a random port.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		Port:                  0,
		ShutdownTimeout:       5 * time.Second,
		HTTPReadHeaderTimeout: 2 * time.Second,
		HTTPReadTimeout:       5 * time.Second,
		HTTPWriteTimeout:      5 * time.Second,
		HTTPIdleTimeout:       5 * time.Second,
		DataDir:               filepath.Join(root, "data"),
		LogDir:                filepath.Join(root, "logs"),
		LogLevel:              "info",
		LogMaxSizeMB:          5,
		LogMaxBackups:         1,
		LogMaxAgeDays:         1,
		CacheCapacity:         1000,
		CacheTTL:              time.Minute,
		CacheJanitorInterval:  20 * time.Millisecond,
	}
}

// server is one running instance of the service.
type server struct {
	t        *testing.T
	cfg      config.Config
	base     string
	cancel   context.CancelFunc
	finished chan struct{}
	runErr   error
	stopped  bool
}

func startServer(t *testing.T, cfg config.Config) *server {
	t.Helper()
	a, err := app.New(cfg, app.WithLogOutput(io.Discard))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{t: t, cfg: cfg, cancel: cancel, finished: make(chan struct{})}
	go func() {
		s.runErr = a.Run(ctx)
		close(s.finished)
	}()
	select {
	case <-a.Started():
	case <-s.finished:
		t.Fatalf("Run returned before starting: %v", s.runErr)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	_, port, err := net.SplitHostPort(a.Addr())
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://127.0.0.1:" + port
	t.Cleanup(func() { s.stop() })
	return s
}

// stop shuts the server down gracefully and checks that it exits cleanly.
// It is safe to call more than once.
func (s *server) stop() {
	s.t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	s.cancel()
	select {
	case <-s.finished:
	case <-time.After(10 * time.Second):
		s.t.Error("server did not stop within 10s")
		return
	}
	if s.runErr != nil {
		s.t.Errorf("Run returned %v after a clean stop request", s.runErr)
	}
}

var client = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true},
}

// loadClient is for background goroutines that send requests in a tight loop.
// Opening a connection per request, as client does, burns thousands of
// ephemeral ports a second; with the packages' tests running in parallel the
// port range fills with TIME_WAIT sockets and the kernel starts dropping SYNs,
// which shows up as "dial tcp: i/o timeout" in unrelated tests. Reusing a few
// connections keeps the load while leaving the ports alone.
var loadClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
	},
}

// response is a received HTTP response with its body already read.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) String() string { return strings.TrimSpace(string(r.Body)) }

// decode unmarshals the body into T, failing the test if it is not valid JSON.
func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("response (status %d) is not the expected JSON: %v\nbody: %s", r.Status, err, r.Body)
	}
	return v
}

// Wire types, deliberately redeclared here rather than imported: the tests
// check the documented JSON, not the server's own structs.
type flagBody struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Overrides   map[string]bool `json:"overrides"`
	// A pointer so a missing field is distinguishable from 0.
	RolloutPercent *int  `json:"rollout_percent"`
	Version        int64 `json:"version"`

	Requires []string `json:"requires"`
}

type evalBody struct {
	Flag    string `json:"flag"`
	UserID  string `json:"user_id"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

type auditEntry struct {
	ID        string    `json:"id"`
	FlagName  string    `json:"flag_name"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Before    *flagBody `json:"before"`
	After     *flagBody `json:"after"`
	RequestID string    `json:"request_id"`
	At        time.Time `json:"at"`
}

type auditBody struct {
	Flag    string       `json:"flag"`
	Entries []auditEntry `json:"entries"`
}

type errorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// call sends one request. A non-nil body is sent as JSON; an empty actor sends
// no X-Actor header. extra is alternating header names and values.
func (s *server) call(method, path, actor string, body any, extra ...string) response {
	s.t.Helper()
	return s.send(client, method, path, actor, body, extra...)
}

// callLoad is call over loadClient, for background loops.
func (s *server) callLoad(method, path, actor string, body any, extra ...string) response {
	s.t.Helper()
	return s.send(loadClient, method, path, actor, body, extra...)
}

func (s *server) send(c *http.Client, method, path, actor string, body any, extra ...string) response {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rdr)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		req.Header.Set("X-Actor", actor)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

func (s *server) mustStatus(r response, want int, what string) response {
	s.t.Helper()
	if r.Status != want {
		s.t.Fatalf("%s: status %d, want %d\nbody: %s", what, r.Status, want, r.Body)
	}
	return r
}

func (s *server) createFlag(name string, enabled bool) flagBody {
	s.t.Helper()
	r := s.mustStatus(s.call("POST", "/v1/flags", "tester", map[string]any{"name": name, "enabled": enabled}), 201, "create "+name)
	return decode[flagBody](s.t, r)
}

func (s *server) updateFlag(name string, patch map[string]any) flagBody {
	s.t.Helper()
	r := s.mustStatus(s.call("PUT", "/v1/flags/"+name, "tester", patch), 200, "update "+name)
	return decode[flagBody](s.t, r)
}

func (s *server) setOverride(name, user string, enabled bool) {
	s.t.Helper()
	s.mustStatus(s.call("PUT", "/v1/flags/"+name+"/overrides/"+user, "tester", map[string]any{"enabled": enabled}), 200, "set override")
}

func (s *server) evaluate(name, user string) evalBody {
	s.t.Helper()
	r := s.mustStatus(s.call("GET", "/v1/evaluate?flag="+name+"&user_id="+user, "", nil), 200, "evaluate "+name+"/"+user)
	return decode[evalBody](s.t, r)
}

func (s *server) audit(name string) []auditEntry {
	s.t.Helper()
	r := s.mustStatus(s.call("GET", "/v1/flags/"+name+"/audit?limit=1000", "", nil), 200, "audit "+name)
	return decode[auditBody](s.t, r).Entries
}

func (s *server) expectEval(name, user string, enabled bool, reason, source string) {
	s.t.Helper()
	got := s.evaluate(name, user)
	if got.Enabled != enabled || (reason != "" && got.Reason != reason) || (source != "" && got.Source != source) {
		s.t.Fatalf("evaluate %s/%s = enabled=%v reason=%s source=%s; want enabled=%v reason=%q source=%q",
			name, user, got.Enabled, got.Reason, got.Source, enabled, reason, source)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}
