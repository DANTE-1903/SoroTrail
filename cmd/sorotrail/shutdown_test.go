// End-to-end lifecycle coverage for the indexer: startup order,
// graceful drain, forced exit on a second signal, and the exit codes
// that scripts and container tooling branch on (issue #716).
//
// Signal disposition and exit codes are process properties, so these
// tests build and run the real binary instead of calling run() in
// process. Database-backed cases follow the repository convention:
// they run against TEST_DATABASE_URL and skip without it.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	lifecycleOnce  sync.Once
	lifecycleBin   string
	lifecycleBinD  string
	lifecycleBinEr error
)

// sorotrailBinary builds the real command once per `go test` run. The
// lifecycle tests need the actual binary because signal disposition
// (default action vs. notify) and exit codes are process properties
// that cannot be observed from inside the test binary.
func sorotrailBinary(t *testing.T) string {
	t.Helper()
	lifecycleOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sorotrail-lifecycle-")
		if err != nil {
			lifecycleBinEr = err
			return
		}
		lifecycleBinD = dir
		path := filepath.Join(dir, "sorotrail")
		cmd := exec.Command("go", "build", "-o", path, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			lifecycleBinEr = fmt.Errorf("building sorotrail: %w\n%s", err, out)
			return
		}
		lifecycleBin = path
	})
	require.NoError(t, lifecycleBinEr, "building the sorotrail binary")
	return lifecycleBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if lifecycleBinD != "" {
		_ = os.RemoveAll(lifecycleBinD)
	}
	os.Exit(code)
}

// testDatabaseURL returns the shared test database or skips the test,
// matching the convention documented in CONTRIBUTING.md: without
// TEST_DATABASE_URL database-backed tests skip rather than fail.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed lifecycle test (see CONTRIBUTING.md)")
	}
	return dbURL
}

// newRPCStub answers the methods the ingester and the /health probe
// call, so the child process runs against a predictable local chain
// instead of the public testnet.
func newRPCStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "getHealth":
			result = map[string]any{"status": "healthy", "latestLedger": 1000, "oldestLedger": 1}
		case "getLatestLedger":
			result = map[string]any{"sequence": 1000}
		case "getEvents":
			result = map[string]any{"latestLedger": 1000, "events": []any{}}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// freeLocalPort reserves a loopback port and releases it, giving the
// child process an address the test can predict.
func freeLocalPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// lifecycleChild is a running sorotrail process plus the pipes that
// capture its output for assertions and failure diagnostics.
type lifecycleChild struct {
	cmd    *exec.Cmd
	addr   string
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	waitEr error
}

// startChild runs the binary with a fully explicit environment so the
// result is reproducible on developer machines and CI alike.
func startChild(t *testing.T, env map[string]string) *lifecycleChild {
	t.Helper()
	c := &lifecycleChild{
		cmd:  exec.Command(sorotrailBinary(t)),
		done: make(chan struct{}),
	}
	c.cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, mapEnv(env)...)
	c.cmd.Stdout = &c.stdout
	c.cmd.Stderr = &c.stderr
	require.NoError(t, c.cmd.Start(), "launching sorotrail")
	go func() {
		c.waitEr = c.cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			// A failed assertion must not leak a live listener.
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

func mapEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// startIndexer launches the full indexer against the stub RPC, the
// test database, and a freshly reserved loopback port.
func startIndexer(t *testing.T, dbURL string, overrides map[string]string) *lifecycleChild {
	t.Helper()
	rpc := newRPCStub(t)
	addr := freeLocalPort(t)
	env := map[string]string{
		"DATABASE_URL": dbURL,
		"RPC_URL":      rpc.URL,
		"HTTP_ADDR":    addr,
		// Long read deadlines keep the held-open connection below
		// inside the server's grace window instead of tripping a
		// header timeout, which would close it and let shutdown
		// finish early.
		"HTTP_READ_TIMEOUT":        "120s",
		"HTTP_READ_HEADER_TIMEOUT": "120s",
		"HTTP_IDLE_TIMEOUT":        "120s",
	}
	for k, v := range overrides {
		env[k] = v
	}
	c := startChild(t, env)
	c.addr = addr
	return c
}

// waitHealthy polls /health until the process serves 200, failing
// with captured output if the process dies or never comes up.
func (c *lifecycleChild) waitHealthy(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-c.done:
			t.Fatalf("process exited before serving /health: %v\nstdout:\n%s\nstderr:\n%s",
				c.waitEr, c.stdout.String(), c.stderr.String())
		default:
		}
		resp, err := client.Get("http://" + c.addr + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/health never became ready\nstdout:\n%s\nstderr:\n%s",
		c.stdout.String(), c.stderr.String())
}

// wait blocks for the process to exit, failing the test on timeout.
func (c *lifecycleChild) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-c.done:
		return c.waitEr
	case <-time.After(timeout):
		t.Fatalf("process still running after %s\nstdout:\n%s\nstderr:\n%s",
			timeout, c.stdout.String(), c.stderr.String())
		return nil
	}
}

// holdOpenConnection parks a connection mid-request: the server has
// read part of the header block and keeps the connection non-idle
// until the request completes or the grace period expires. That is the
// in-flight work a graceful shutdown has to account for.
func holdOpenConnection(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Write([]byte("GET /stats HTTP/1.1\r\nHost: localhost\r\nX-Hold-Open: 1"))
	require.NoError(t, err)
}

// assertLogOrder fails unless every marker appears in out in the given
// relative order.
func assertLogOrder(t *testing.T, out string, markers ...string) {
	t.Helper()
	last := -1
	lastMarker := ""
	for _, m := range markers {
		idx := strings.Index(out, m)
		require.Greaterf(t, idx, -1, "log output is missing %q\nstdout:\n%s", m, out)
		require.Greaterf(t, idx, last, "log line %q appears before %q\nstdout:\n%s", m, lastMarker, out)
		last, lastMarker = idx, m
	}
}

// TestExitCodeReflectsProcessOutcome covers main's exit-code mapping:
// scripts and container tooling branch on 2 (interrupted — re-run to
// resume) versus 1 (failed), so the mapping is part of the interface.
func TestExitCodeReflectsProcessOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "clean run exits 0", err: nil, want: 0},
		{name: "interrupted run exits 2", err: errInterrupted, want: 2},
		{name: "wrapped interrupt still exits 2", err: fmt.Errorf("replay: %w", errInterrupted), want: 2},
		{name: "any other failure exits 1", err: errors.New("connecting to postgres: refused"), want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, exitCode(c.err))
		})
	}
}

// TestBadConfigExitsOne runs the real binary with a broken environment:
// main must print the error to stderr and exit 1, the observable
// contract that tells an orchestrator "this will never become healthy
// on its own; read the log".
func TestBadConfigExitsOne(t *testing.T) {
	c := startChild(t, map[string]string{})
	require.Error(t, c.wait(t, 30*time.Second),
		"a misconfigured process must not exit 0\nstdout:\n%s\nstderr:\n%s",
		c.stdout.String(), c.stderr.String())
	assert.Equal(t, 1, c.cmd.ProcessState.ExitCode())
	assert.Contains(t, c.stderr.String(), "sorotrail:")
	assert.Contains(t, c.stderr.String(), "DATABASE_URL")
}

// TestStartupShutdownSequence runs the indexer end to end and asserts
// both halves of the lifecycle as one sequence: components start in an
// order that satisfies their dependencies, a single signal drains them
// within the grace period (including an in-flight connection), every
// component reports a clean stop, and the process exits 0.
func TestStartupShutdownSequence(t *testing.T) {
	dbURL := testDatabaseURL(t)

	const grace = 2 * time.Second
	c := startIndexer(t, dbURL, map[string]string{"SHUTDOWN_TIMEOUT": grace.String()})
	c.waitHealthy(t)

	// Park an in-flight connection so the drain has real work to wait
	// for; without it the test would pass even if shutdown ignored
	// active connections entirely.
	holdOpenConnection(t, c.addr)

	sendSignal(t, c)
	signalAt := time.Now()
	require.NoError(t, c.wait(t, grace+30*time.Second),
		"graceful shutdown must finish\nstdout:\n%s\nstderr:\n%s",
		c.stdout.String(), c.stderr.String())
	elapsed := time.Since(signalAt)

	assert.Equal(t, 0, c.cmd.ProcessState.ExitCode(),
		"a clean shutdown must exit 0\nstdout:\n%s", c.stdout.String())
	assert.GreaterOrEqual(t, elapsed, grace/2,
		"shutdown must wait for the in-flight connection, not drop it on the first signal")
	assert.Less(t, elapsed, grace+10*time.Second,
		"shutdown must be bounded by the grace period")

	stdout := c.stdout.String()
	// Strict order of the main goroutine's own log lines: config is
	// read, the store is up, and the process only winds down after
	// the signal arrives.
	assertLogOrder(t, stdout,
		"startup configuration",
		"postgres connection established",
		"shutdown signal received",
		"shutdown complete",
	)
	// Both long-running components announced themselves before the
	// signal, i.e. they started on live configuration rather than
	// during teardown.
	signalIdx := strings.Index(stdout, "shutdown signal received")
	for _, started := range []string{"http api listening", "ingester starting"} {
		idx := strings.Index(stdout, started)
		require.Greaterf(t, idx, -1, "stdout:\n%s", stdout)
		require.Lessf(t, idx, signalIdx, "%q must be logged before the signal arrives\nstdout:\n%s", started, stdout)
	}
	// Every component reported its stop before the process declared
	// shutdown complete. The stop logs race each other (all wake on
	// the same context cancellation), but the drain's accounting
	// guarantees each report arrives before the final line — this is
	// exactly what used to hang: the drain waited on a webhook report
	// that never came.
	completeIdx := strings.Index(stdout, "shutdown complete")
	require.Greaterf(t, completeIdx, signalIdx, "stdout:\n%s", stdout)
	for _, stopped := range []string{"webhook delivery workers stopping", "ingester stopped"} {
		idx := strings.Index(stdout, stopped)
		require.Greaterf(t, idx, -1, "stdout:\n%s", stdout)
		require.Lessf(t, idx, completeIdx,
			"%q must be logged before shutdown complete\nstdout:\n%s", stopped, stdout)
	}
}

// TestSecondSignalForcesImmediateExit asserts the escape hatch: when
// the grace period would otherwise sit on a stuck drain, a second
// SIGINT must take the runtime's default action and terminate the
// process at once instead of being swallowed.
func TestSecondSignalForcesImmediateExit(t *testing.T) {
	dbURL := testDatabaseURL(t)

	// A long grace period makes the distinction observable: without
	// the second-signal behaviour the process would sit here for the
	// full 30 seconds.
	c := startIndexer(t, dbURL, map[string]string{"SHUTDOWN_TIMEOUT": "30s"})
	c.waitHealthy(t)
	holdOpenConnection(t, c.addr)

	sendSignal(t, c)
	// Give the first signal time to enter the shutdown path and
	// unregister the handler before the second one arrives.
	time.Sleep(200 * time.Millisecond)
	sendSignal(t, c)
	secondAt := time.Now()

	err := c.wait(t, 10*time.Second)
	elapsed := time.Since(secondAt)
	require.Error(t, err,
		"the second signal must terminate the process before the grace period ends\nstdout:\n%s\nstderr:\n%s",
		c.stdout.String(), c.stderr.String())
	assert.Less(t, elapsed, 5*time.Second,
		"exit must follow the second signal promptly, not the grace period")

	ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok, "expected a unix wait status")
	assert.True(t, ws.Signaled(), "expected death by signal, got %s", c.cmd.ProcessState.String())
	if ws.Signaled() {
		assert.Equal(t, syscall.SIGINT, ws.Signal())
	}
	// The first signal did begin the graceful path; it is the second
	// one that forced the exit.
	assert.Contains(t, c.stdout.String(), "shutdown signal received")
}

// TestComponentFailureExitsOne asserts the other end of the exit-code
// contract: when a component fails during startup (here: the HTTP
// server cannot bind), the process drains the rest, reports the
// failure on stderr, and exits 1 rather than 0.
func TestComponentFailureExitsOne(t *testing.T) {
	dbURL := testDatabaseURL(t)

	// Occupy a port so ListenAndServe fails with "address already in
	// use" the moment the server goroutine starts.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()

	c := startIndexer(t, dbURL, map[string]string{"HTTP_ADDR": blocker.Addr().String()})
	require.Error(t, c.wait(t, 30*time.Second),
		"a failed component must not exit 0\nstdout:\n%s\nstderr:\n%s",
		c.stdout.String(), c.stderr.String())
	assert.Equal(t, 1, c.cmd.ProcessState.ExitCode(),
		"stdout:\n%s", c.stdout.String())
	assert.Contains(t, c.stderr.String(), "http server:")
	assert.Contains(t, c.stdout.String(), "shutdown complete",
		"the process must drain the remaining components before exiting\nstdout:\n%s",
		c.stdout.String())
}

// sendSignal delivers SIGINT to the child.
func sendSignal(t *testing.T, c *lifecycleChild) {
	t.Helper()
	require.NoError(t, c.cmd.Process.Signal(os.Interrupt))
}
