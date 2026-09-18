package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDualModeSmoke is Station's own acceptance test: the real station
// binary, using its own real station.yaml plugins (echo-server,
// clock-plugin — one process-mode, one inprocess-mode), running
// simultaneously, each independently reachable, each recovering from a
// real kill of its own OS process. This is the "two working virtual
// MCPs" proof that mcp-host is feature-complete enough to build a real
// prototype on.
func TestDualModeSmoke(t *testing.T) {
	stationBin := buildStationBinary(t)
	echoBin := buildBinary(t, "github.com/hollis-labs/station/plugins/echo-server", "echo-server")
	clockBin := buildBinary(t, "github.com/hollis-labs/station/plugins/clock-plugin", "clock-plugin")

	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	configPath := filepath.Join(t.TempDir(), "station.yaml")
	configYAML := fmt.Sprintf(`
logical_servers:
  - id: echo
    name: Echo Server
    transport: process
    process:
      command: %q
    serve:
      stdio: true
  - id: clock
    name: Clock Plugin
    transport: inprocess
    inprocess:
      command: %q
      tools:
        - name: now
          description: Returns the current time.
          input_schema:
            type: object
            properties: {}
          annotations:
            readOnlyHint: true
    serve:
      http:
        path: /clock
`, echoBin, clockBin)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	stationCmd := exec.CommandContext(ctx, stationBin, "-config", configPath, "-http-addr", httpAddr)
	watcher := newStderrPIDWatcher()
	watcher.attach(t, stationCmd)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "smoke-test", Version: "1"}, nil)
	stdioSession, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: stationCmd}, nil)
	if err != nil {
		t.Fatalf("connect to station over stdio: %v", err)
	}
	defer stdioSession.Close()

	// --- Echo: process-mode, served over Station's own stdio. ---
	echoTools, err := stdioSession.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("echo tools/list: %v", err)
	}
	if len(echoTools.Tools) != 1 || echoTools.Tools[0].Name != "echo" {
		t.Fatalf("echo tools/list = %+v, want one tool named echo", echoTools.Tools)
	}
	if err := callEcho(ctx, stdioSession); err != nil {
		t.Fatalf("initial echo call: %v", err)
	}

	// --- Clock: inprocess-mode, served over HTTP. ---
	clockURL := "http://" + httpAddr + "/clock"
	waitForHTTPReachable(t, ctx, clockURL)

	clockSession := connectHTTP(t, ctx, clockURL)
	defer clockSession.Close()

	clockTools, err := clockSession.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("clock tools/list: %v", err)
	}
	if len(clockTools.Tools) != 1 || clockTools.Tools[0].Name != "now" {
		t.Fatalf("clock tools/list = %+v, want one tool named now", clockTools.Tools)
	}
	if err := callNow(ctx, clockSession); err != nil {
		t.Fatalf("initial clock call: %v", err)
	}

	// --- Kill echo's real OS process; confirm supervision recovers it
	// without the client ever reconnecting to Station itself. ---
	echoPID := waitForPID(t, watcher, "echo", time.Now().Add(10*time.Second))
	if err := syscall.Kill(echoPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill echo child (pid %d): %v", echoPID, err)
	}
	pollUntilRecovered(t, "echo", func() error { return callEcho(ctx, stdioSession) })

	// --- Kill clock's real OS process; confirm supervision recovers it
	// the same way. ---
	clockPID := waitForPID(t, watcher, "clock", time.Now().Add(10*time.Second))
	if err := syscall.Kill(clockPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill clock child (pid %d): %v", clockPID, err)
	}
	pollUntilRecovered(t, "clock", func() error { return callNow(ctx, clockSession) })
}

func buildBinary(t *testing.T, pkg, name string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binPath, pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return binPath
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func callEcho(ctx context.Context, session *sdkmcp.ClientSession) error {
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "echo", Arguments: map[string]any{"message": "smoke"}})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("echo tool returned IsError: %+v", res.Content)
	}
	return nil
}

func callNow(ctx context.Context, session *sdkmcp.ClientSession) error {
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "now", Arguments: map[string]any{}})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("now tool returned IsError: %+v", res.Content)
	}
	return nil
}

func connectHTTP(t *testing.T, ctx context.Context, url string) *sdkmcp.ClientSession {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "smoke-test-http", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect to %s: %v", url, err)
	}
	return session
}

func waitForHTTPReachable(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodOptions, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("http endpoint %s did not become reachable in time", url)
}

func pollUntilRecovered(t *testing.T, label string, attempt func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := attempt(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not recover within deadline: %v", label, lastErr)
}

// pidLineRe matches mcp-host's own "process spawned" / "plugin
// subprocess spawned" log lines (slog's text handler: key=value pairs),
// e.g. `... msg="station: process spawned" server=echo pid=12345`.
var pidLineRe = regexp.MustCompile(`server=(\S+).*\bpid=(\d+)`)

// stderrPIDWatcher tails a station process's stderr, tracking the most
// recently logged pid for each named logical server — the initial spawn
// and every later respawn overwrite it — while relaying every line to
// the test log for failure diagnostics.
type stderrPIDWatcher struct {
	mu  sync.Mutex
	pid map[string]int
}

func newStderrPIDWatcher() *stderrPIDWatcher { return &stderrPIDWatcher{pid: map[string]int{}} }

func (w *stderrPIDWatcher) attach(t *testing.T, cmd *exec.Cmd) {
	pr, pw := io.Pipe()
	cmd.Stderr = pw
	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Text()
			t.Log("[station] " + line)
			if m := pidLineRe.FindStringSubmatch(line); m != nil {
				if pid, err := strconv.Atoi(m[2]); err == nil {
					w.mu.Lock()
					w.pid[m[1]] = pid
					w.mu.Unlock()
				}
			}
		}
	}()
}

func (w *stderrPIDWatcher) pidFor(server string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pid[server]
}

func waitForPID(t *testing.T, w *stderrPIDWatcher, server string, deadline time.Time) int {
	t.Helper()
	for time.Now().Before(deadline) {
		if pid := w.pidFor(server); pid != 0 {
			return pid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q's pid to appear in station's logs", server)
	return 0
}
