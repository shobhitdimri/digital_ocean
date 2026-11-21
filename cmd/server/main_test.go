package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const reexecEnv = "FFS_TEST_RUN_MAIN"

// TestMain lets the test binary double as the server: the tests below start
// themselves with reexecEnv set, so they exercise the real main() and real
// operating-system signals without needing a prebuilt binary.
func TestMain(m *testing.M) {
	if os.Getenv(reexecEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

type child struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	exited chan error
	port   int
	dir    string
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func spawn(t *testing.T, port int, extraEnv ...string) *child {
	t.Helper()
	dir := t.TempDir()
	c := &child{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, exited: make(chan error, 1), port: port, dir: dir}
	c.cmd = exec.Command(os.Args[0])
	c.cmd.Env = append(os.Environ(),
		reexecEnv+"=1",
		"GORACE=atexit_sleep_ms=0", // the race detector otherwise sleeps 1s at exit
		fmt.Sprintf("PORT=%d", port),
		"DATA_DIR="+dir+"/data",
		"LOG_DIR="+dir+"/logs",
	)
	c.cmd.Env = append(c.cmd.Env, extraEnv...)
	c.cmd.Stdout, c.cmd.Stderr = c.stdout, c.stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.exited <- c.cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-c.exited:
		default:
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	})
	return c
}

func (c *child) waitHealthy(t *testing.T) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", c.port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.exited:
			t.Fatalf("server exited early: %v\nstdout:\n%s\nstderr:\n%s", err, c.stdout, c.stderr)
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server never became ready\nstdout:\n%s\nstderr:\n%s", c.stdout, c.stderr)
}

func (c *child) waitExit(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-c.exited:
		c.exited <- err
		return err
	case <-time.After(d):
		t.Fatalf("process did not exit within %s\nstdout:\n%s", d, c.stdout)
		return nil
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func TestSignalsTriggerAGracefulExitWithStatusZero(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			c := spawn(t, freePort(t))
			c.waitHealthy(t)

			if err := c.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := exitCode(c.waitExit(t, 10*time.Second)); code != 0 {
				t.Fatalf("exit code %d, want 0\nstdout:\n%s\nstderr:\n%s", code, c.stdout, c.stderr)
			}
			out := c.stdout.String()
			for _, want := range []string{"starting server", "shutdown signal received", "server stopped cleanly"} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
			if c.stderr.Len() != 0 {
				t.Errorf("a clean shutdown wrote to stderr: %s", c.stderr)
			}
			if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", c.port)); err == nil {
				t.Error("the port still answers after exit")
			}
		})
	}
}

func TestShutdownDelayKeepsAnsweringWhileReportingNotReady(t *testing.T) {
	c := spawn(t, freePort(t), "SHUTDOWN_DELAY=1500ms")
	c.waitHealthy(t)

	began := time.Now()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	var sawNotReady, stillLive bool
	for time.Since(began) < 1200*time.Millisecond {
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", c.port)); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				sawNotReady = true
			}
		}
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", c.port)); err == nil {
			resp.Body.Close()
			stillLive = resp.StatusCode == 200
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawNotReady || !stillLive {
		t.Fatalf("during the delay want /readyz 503 (got %v) and /healthz 200 (got %v)", sawNotReady, stillLive)
	}
	if code := exitCode(c.waitExit(t, 10*time.Second)); code != 0 {
		t.Fatalf("exit code %d\n%s", code, c.stdout)
	}
	if took := time.Since(began); took < 1500*time.Millisecond {
		t.Fatalf("exited after %s, before the 1.5s delay elapsed", took)
	}
}

func TestStartupFailuresExitNonZeroWithAFatalMessage(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	busyPort := occupied.Addr().(*net.TCPAddr).Port

	tests := []struct {
		name string
		port int
		env  []string
	}{
		{"port already in use", busyPort, nil},
		{"unparsable config", freePort(t), []string{"PORT=not-a-number"}},
		{"out of range port", freePort(t), []string{"PORT=70000"}},
		{"negative shutdown delay", freePort(t), []string{"SHUTDOWN_DELAY=-1s"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := spawn(t, tc.port, tc.env...)
			code := exitCode(c.waitExit(t, 10*time.Second))
			if code != 1 {
				t.Fatalf("exit code %d, want 1\nstdout:\n%s\nstderr:\n%s", code, c.stdout, c.stderr)
			}
			if !strings.Contains(c.stderr.String(), "fatal:") {
				t.Fatalf("stderr lacks a fatal line: %q", c.stderr)
			}
		})
	}
}

func TestSecondSignalDoesNotBreakAnInProgressShutdown(t *testing.T) {
	c := spawn(t, freePort(t), "SHUTDOWN_DELAY=800ms")
	c.waitHealthy(t)

	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = c.cmd.Process.Signal(syscall.SIGTERM)

	if code := exitCode(c.waitExit(t, 10*time.Second)); code != 0 {
		t.Fatalf("exit code %d after a repeated signal\n%s", code, c.stdout)
	}
	if n := strings.Count(c.stdout.String(), "shutdown signal received"); n != 1 {
		t.Fatalf("shutdown began %d times, want exactly once", n)
	}
}
