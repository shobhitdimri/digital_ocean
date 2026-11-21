package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"feature-flag-service/internal/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Port:                  0,
		ShutdownTimeout:       3 * time.Second,
		HTTPReadHeaderTimeout: 2 * time.Second,
		HTTPReadTimeout:       5 * time.Second,
		HTTPWriteTimeout:      5 * time.Second,
		HTTPIdleTimeout:       5 * time.Second,
		DataDir:               filepath.Join(t.TempDir(), "data"),
		LogDir:                filepath.Join(t.TempDir(), "logs"),
		LogLevel:              "info",
		LogMaxSizeMB:          5,
		LogMaxBackups:         1,
		LogMaxAgeDays:         1,
		CacheCapacity:         100,
		CacheTTL:              time.Minute,
		CacheJanitorInterval:  20 * time.Millisecond,
	}
}

type running struct {
	t        *testing.T
	app      *App
	cancel   context.CancelFunc
	finished chan struct{} // closed when Run has returned
	err      error         // Run's result, valid once finished is closed
	base     string
}

// start builds and runs an App on a random port. wrap, if given, wraps the
// root handler so a test can add routes such as a deliberately slow one.
func start(t *testing.T, cfg config.Config, wrap func(http.Handler) http.Handler) *running {
	t.Helper()
	a, err := New(cfg, WithLogOutput(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if wrap != nil {
		a.srv.Handler = wrap(a.srv.Handler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{t: t, app: a, cancel: cancel, finished: make(chan struct{})}
	go func() {
		r.err = a.Run(ctx)
		close(r.finished)
	}()

	select {
	case <-a.Started():
	case <-r.finished:
		cancel()
		t.Fatalf("Run returned before starting: %v", r.err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	_, port, err := net.SplitHostPort(a.Addr())
	if err != nil {
		t.Fatal(err)
	}
	r.base = "http://127.0.0.1:" + port
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.finished:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return during cleanup")
		}
	})
	return r
}

var noKeepAlive = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true},
}

func (r *running) get(path string) (int, string) {
	r.t.Helper()
	resp, err := noKeepAlive.Get(r.base + path)
	if err != nil {
		r.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func (r *running) post(path, actor, body string) int {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", actor)
	resp, err := noKeepAlive.Do(req)
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (r *running) waitRun(d time.Duration) error {
	r.t.Helper()
	select {
	case <-r.finished:
		return r.err
	case <-time.After(d):
		r.t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

func (r *running) runReturned() bool {
	select {
	case <-r.finished:
		return true
	default:
		return false
	}
}

func logFile(t *testing.T, cfg config.Config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.LogDir, "app.log"))
	if err != nil {
		t.Fatalf("read app.log: %v", err)
	}
	return string(b)
}

func goroutinesContaining(name string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), name)
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

func refused(base string) bool {
	host := strings.TrimPrefix(base, "http://")
	c, err := net.DialTimeout("tcp", host, 200*time.Millisecond)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

// slowRoute adds GET /slow, which signals when a request arrives and then
// blocks until release is closed.
func slowRoute(entered chan<- struct{}, release <-chan struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
			io.WriteString(w, "finished")
		})
		mux.Handle("/", next)
		return mux
	}
}

func TestServesRequestsThenStopsCleanlyOnCancel(t *testing.T) {
	cfg := testConfig(t)
	r := start(t, cfg, nil)

	if !r.app.Ready() {
		t.Fatal("app not ready while serving")
	}
	if code, body := r.get("/healthz"); code != 200 || body != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %s", code, body)
	}
	if code, body := r.get("/readyz"); code != 200 || body != `{"status":"ready"}` {
		t.Fatalf("readyz = %d %s", code, body)
	}

	r.cancel()
	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatalf("Run = %v, want nil after a clean shutdown", err)
	}
	if !refused(r.base) {
		t.Fatal("still accepting connections after Run returned")
	}
	if r.app.Ready() {
		t.Fatal("app still reports ready after shutdown")
	}

	logs := logFile(t, cfg)
	var order []string
	for _, marker := range []string{"starting server", "shutdown signal received", "server stopped cleanly"} {
		i := strings.Index(logs, marker)
		if i < 0 {
			t.Fatalf("log lacks %q:\n%s", marker, logs)
		}
		order = append(order, marker)
		if len(order) > 1 && i < strings.Index(logs, order[len(order)-2]) {
			t.Fatalf("log lines out of order:\n%s", logs)
		}
	}
}

func TestInFlightRequestFinishesDuringShutdownAndNewOnesAreRefused(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	r := start(t, testConfig(t), slowRoute(entered, release))

	type result struct {
		code int
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := noKeepAlive.Get(r.base + "/slow")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res <- result{code: resp.StatusCode, body: string(b)}
	}()
	<-entered

	r.cancel()
	waitUntil(t, 3*time.Second, "the listener to close", func() bool { return refused(r.base) })

	select {
	case got := <-res:
		t.Fatalf("in-flight request ended early: %+v", got)
	default:
	}
	if r.runReturned() {
		t.Fatal("Run returned while a request was still in flight")
	}

	close(release)
	got := <-res
	if got.err != nil || got.code != 200 || got.body != "finished" {
		t.Fatalf("in-flight request was not completed: %+v", got)
	}
	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestReadyzReportsNotReadyWhileDraining(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownDelay = 500 * time.Millisecond
	r := start(t, cfg, nil)

	if code, _ := r.get("/readyz"); code != 200 {
		t.Fatalf("readyz before shutdown = %d", code)
	}

	began := time.Now()
	r.cancel()
	waitUntil(t, 400*time.Millisecond, "readyz to report 503", func() bool {
		resp, err := noKeepAlive.Get(r.base + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusServiceUnavailable
	})
	if code, body := r.get("/readyz"); code != 503 || body != `{"status":"shutting_down"}` {
		t.Fatalf("readyz while draining = %d %s", code, body)
	}
	if code, _ := r.get("/healthz"); code != 200 {
		t.Fatalf("liveness must stay 200 while draining, got %d", code)
	}
	if code, _ := r.get("/v1/flags"); code != 200 {
		t.Fatalf("the API should still serve during the drain delay, got %d", code)
	}

	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed < cfg.ShutdownDelay {
		t.Fatalf("stopped after %s, before the %s drain delay elapsed", elapsed, cfg.ShutdownDelay)
	}
	if !refused(r.base) {
		t.Fatal("still listening after shutdown")
	}
}

func TestJanitorGoroutineStopsWhenRunReturns(t *testing.T) {
	r := start(t, testConfig(t), nil)
	if !goroutinesContaining("runJanitor") {
		t.Fatal("test is not observing the janitor: no runJanitor goroutine while the app runs")
	}
	r.cancel()
	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "the janitor goroutine to exit", func() bool { return !goroutinesContaining("runJanitor") })
	if goroutinesContaining("(*Server).Serve") {
		t.Fatal("the serving goroutine outlived Run")
	}
}

func TestRunFailsWhenThePortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	cfg := testConfig(t)
	cfg.Port = taken.Addr().(*net.TCPAddr).Port
	a, err := New(cfg, WithLogOutput(io.Discard))
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	err = a.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded on an occupied port")
	}
	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("err = %v, want an address-in-use error", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("a bind failure must be reported immediately")
	}
	select {
	case <-a.Started():
		t.Fatal("Started closed although the server never listened")
	default:
	}
	if goroutinesContaining("runJanitor") {
		t.Fatal("resources were not released after a failed start")
	}
	if !strings.Contains(logFile(t, cfg), "cannot listen") {
		t.Fatal("the bind failure was not logged")
	}
}

func TestDrainTimeoutForcesCloseAndReportsIt(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownTimeout = 150 * time.Millisecond
	entered, release := make(chan struct{}, 1), make(chan struct{})
	r := start(t, cfg, slowRoute(entered, release))
	defer close(release)

	clientErr := make(chan error, 1)
	go func() {
		resp, err := noKeepAlive.Get(r.base + "/slow")
		if err == nil {
			resp.Body.Close()
		}
		clientErr <- err
	}()
	<-entered

	began := time.Now()
	r.cancel()
	err := r.waitRun(5 * time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "graceful shutdown") {
		t.Fatalf("Run = %v, want a graceful-shutdown deadline error", err)
	}
	if elapsed := time.Since(began); elapsed < cfg.ShutdownTimeout || elapsed > 3*time.Second {
		t.Fatalf("forced close after %s, want about %s", elapsed, cfg.ShutdownTimeout)
	}
	select {
	case err := <-clientErr:
		if err == nil {
			t.Fatal("the stuck request succeeded; it should have been cut off")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stuck client connection was not closed")
	}
	if !strings.Contains(logFile(t, cfg), "forcing close") {
		t.Fatal("the forced close was not logged")
	}
	waitUntil(t, 2*time.Second, "the janitor to stop", func() bool { return !goroutinesContaining("runJanitor") })
}

func TestRunCannotBeCalledTwice(t *testing.T) {
	r := start(t, testConfig(t), nil)
	if err := r.app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("second Run = %v", err)
	}
	if code, _ := r.get("/healthz"); code != 200 {
		t.Fatal("a rejected second Run disturbed the running server")
	}
}

func TestNewFailuresAreReportedAndCleanedUp(t *testing.T) {
	dataIsAFile := func(c *config.Config) {
		f := filepath.Join(t.TempDir(), "occupied")
		os.WriteFile(f, []byte("x"), 0o600)
		c.DataDir = filepath.Join(f, "sub")
	}
	corruptStore := func(c *config.Config) {
		os.MkdirAll(c.DataDir, 0o755)
		os.WriteFile(filepath.Join(c.DataDir, "flags.json"), []byte("{not json"), 0o600)
	}
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"bad log level", func(c *config.Config) { c.LogLevel = "chatty" }, "init logger"},
		{"data dir unusable", dataIsAFile, "init flag store"},
		{"corrupt flag store", corruptStore, "corrupt"},
		{"invalid cache capacity", func(c *config.Config) { c.CacheCapacity = 0 }, "init cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			tt.mutate(&cfg)
			a, err := New(cfg, WithLogOutput(io.Discard))
			if err == nil || a != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New = (%v, %v), want an error containing %q", a, err, tt.want)
			}
			if goroutinesContaining("runJanitor") {
				t.Fatal("a failed New left a janitor goroutine running")
			}
		})
	}
}

type recordingCloser struct {
	name  string
	order *[]string
	err   error
}

func (c recordingCloser) Close() error {
	*c.order = append(*c.order, c.name)
	return c.err
}

func TestClosersRunInReverseOrderAndAllRunEvenAfterAnError(t *testing.T) {
	var order []string
	first, second := errors.New("first failure"), errors.New("second failure")
	a := &App{}
	a.closers = []io.Closer{
		recordingCloser{"logger", &order, nil},
		recordingCloser{"cache", &order, second},
		recordingCloser{"other", &order, first},
	}
	err := a.closeAll()
	if got := strings.Join(order, ","); got != "other,cache,logger" {
		t.Fatalf("close order = %s, want other,cache,logger (reverse of creation)", got)
	}
	if !errors.Is(err, first) {
		t.Fatalf("err = %v, want the first failure", err)
	}
}

func TestRealStackIsWiredAndDataSurvivesARestart(t *testing.T) {
	cfg := testConfig(t)

	r := start(t, cfg, nil)
	if code := r.post("/v1/flags", "alice", `{"name":"durable","description":"survives","enabled":true}`); code != 201 {
		t.Fatalf("create = %d", code)
	}
	if code, body := r.get("/v1/evaluate?flag=durable&user_id=u1"); code != 200 || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("evaluate = %d %s", code, body)
	}
	r.cancel()
	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	r2 := start(t, cfg, nil)
	code, body := r2.get("/v1/flags/durable")
	if code != 200 {
		t.Fatalf("flag lost across restart: %d %s", code, body)
	}
	var flag struct {
		Name, Description string
		Enabled           bool
		Version           int
	}
	json.Unmarshal([]byte(body), &flag)
	if flag.Name != "durable" || flag.Description != "survives" || !flag.Enabled || flag.Version != 1 {
		t.Fatalf("restored flag = %+v", flag)
	}
	code, body = r2.get("/v1/flags/durable/audit")
	if code != 200 || !strings.Contains(body, `"actor":"alice"`) || !strings.Contains(body, `"action":"flag.created"`) {
		t.Fatalf("audit lost across restart: %d %s", code, body)
	}
	code, body = r2.get("/v1/evaluate?flag=durable&user_id=u1")
	if code != 200 || !strings.Contains(body, `"source":"store"`) {
		t.Fatalf("a fresh process must evaluate from the store, not an old cache: %d %s", code, body)
	}
}

func TestShutdownWhileIdleIsFast(t *testing.T) {
	cfg := testConfig(t)
	r := start(t, cfg, nil)
	for i := 0; i < 5; i++ {
		r.get("/healthz")
	}
	began := time.Now()
	r.cancel()
	if err := r.waitRun(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("an idle server took %s to stop", elapsed)
	}
}

func TestConcurrentClientsDuringShutdownGetAnswerOrRefusalNeverAHang(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownDelay = 50 * time.Millisecond
	r := start(t, cfg, nil)

	const clients = 20
	var answered, refusedCount, unexpected atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := noKeepAlive.Get(r.base + "/v1/flags")
				if err != nil {
					refusedCount.Add(1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode == 200 {
					answered.Add(1)
				} else {
					unexpected.Add(1)
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	r.cancel()
	if err := r.waitRun(8 * time.Second); err != nil {
		t.Fatalf("Run = %v under load", err)
	}
	close(stop)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("clients did not finish: a request was left hanging")
	}
	if unexpected.Load() != 0 {
		t.Fatalf("%d requests got a status other than 200 during shutdown", unexpected.Load())
	}
	if answered.Load() == 0 {
		t.Fatal("no request succeeded before shutdown")
	}
	t.Logf("%d requests answered, %d clients saw the listener close", answered.Load(), refusedCount.Load())
}

// faultyListener wraps a real listener. Accept blocks until fail is closed,
// then returns an error that is not a normal shutdown; Close unblocks it.
type faultyListener struct {
	net.Listener
	fail   chan struct{}
	closed chan struct{}
	once   sync.Once

	// acceptExitDelay slows Accept's return after Close, simulating a serve
	// goroutine that takes a while to wind down; acceptReturned records that it has.
	acceptExitDelay time.Duration
	acceptReturned  atomic.Bool
	acceptEntered   atomic.Bool
}

var errAcceptBoom = errors.New("accept boom")

func newFaultyListener(inner net.Listener) *faultyListener {
	return &faultyListener{Listener: inner, fail: make(chan struct{}), closed: make(chan struct{})}
}

func (f *faultyListener) Accept() (net.Conn, error) {
	f.acceptEntered.Store(true)
	defer f.acceptReturned.Store(true)
	select {
	case <-f.fail:
		return nil, errAcceptBoom
	case <-f.closed:
		time.Sleep(f.acceptExitDelay)
		return nil, net.ErrClosed
	}
}

func (f *faultyListener) Close() error {
	f.once.Do(func() { close(f.closed) })
	return f.Listener.Close()
}

// listenerRef gives a test access to the faulty listener Run ends up using.
type listenerRef struct {
	v atomic.Pointer[faultyListener]
}

func (r *listenerRef) fail(t *testing.T) {
	t.Helper()
	fl := r.v.Load()
	if fl == nil {
		t.Fatal("the app has not bound a listener yet")
	}
	close(fl.fail)
}

func faultyApp(t *testing.T, cfg config.Config) (*App, *listenerRef) {
	t.Helper()
	a, err := New(cfg, WithLogOutput(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	ref := &listenerRef{}
	a.listen = func(network, addr string) (net.Listener, error) {
		inner, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		fl := newFaultyListener(inner)
		ref.v.Store(fl)
		return fl, nil
	}
	return a, ref
}

func TestServerFailureStopsTheAppReleasesResourcesAndIsReported(t *testing.T) {
	cfg := testConfig(t)
	a, ref := faultyApp(t, cfg)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	<-a.Started()
	ref.fail(t)

	select {
	case err := <-done:
		if !errors.Is(err, errAcceptBoom) || !strings.Contains(err.Error(), "server failed") {
			t.Fatalf("Run = %v, want the server failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not notice that the server failed")
	}
	if a.Ready() {
		t.Fatal("app still ready after the server failed")
	}
	waitUntil(t, 2*time.Second, "the janitor to stop", func() bool { return !goroutinesContaining("runJanitor") })
	if !strings.Contains(logFile(t, cfg), "server failed") {
		t.Fatal("the failure was not logged")
	}
}

func TestServerFailureWhileDrainingStillShutsDownCleanly(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownDelay = 5 * time.Second
	a, ref := faultyApp(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-a.Started()

	cancel()
	waitUntil(t, 2*time.Second, "readiness to flip", func() bool { return !a.Ready() })
	ref.fail(t)

	began := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil: we were already shutting down", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a failure during the drain delay did not cut the wait short")
	}
	if time.Since(began) > 2*time.Second {
		t.Fatalf("waited %s of a 5s delay after the server had already failed", time.Since(began))
	}
	if !strings.Contains(logFile(t, cfg), "server failed while draining") {
		t.Fatal("the drain-time failure was not logged")
	}
}

func TestResourceCloseErrorIsReportedAfterACleanShutdown(t *testing.T) {
	r := start(t, testConfig(t), nil)
	boom := errors.New("flush failed")
	r.app.closers = append(r.app.closers, recordingCloser{name: "extra", order: new([]string), err: boom})

	r.cancel()
	err := r.waitRun(5 * time.Second)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "close resources") {
		t.Fatalf("Run = %v, want the close failure", err)
	}
	waitUntil(t, 2*time.Second, "the janitor to stop", func() bool { return !goroutinesContaining("runJanitor") })
}

func TestRunWaitsForTheServeGoroutineBeforeReleasingResources(t *testing.T) {
	a, ref := faultyApp(t, testConfig(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-a.Started()
	fl := ref.v.Load()
	waitUntil(t, 2*time.Second, "the server to block in Accept", fl.acceptEntered.Load)
	fl.acceptExitDelay = 300 * time.Millisecond

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !fl.acceptReturned.Load() {
		t.Fatal("Run returned while the serve goroutine was still running")
	}
}

func TestLoggerIsRegisteredFirstSoItIsClosedLast(t *testing.T) {
	a, err := New(testConfig(t), WithLogOutput(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.closeAll() })
	if len(a.closers) < 2 {
		t.Fatalf("expected at least the logger and the cache, got %d closers", len(a.closers))
	}
	if a.closers[0] != io.Closer(a.log) {
		t.Fatalf("first registered closer is %T, want the logger, which must be closed last", a.closers[0])
	}
}
