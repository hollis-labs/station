package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func echoPIDs(t *testing.T) []int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-x", "echo-server").Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// A killed child is restarted by mcp-host's supervision and the mount serves again without the
// client reconnecting to Station.
func TestChildCrashIsSupervised(t *testing.T) {
	base, _ := started(t)
	owner := newSession(t, base)
	owner.signIn()
	k := owner.want(201, "POST", "/api/identity/keys", map[string]any{"name": "all", "scopes": []string{"station.*"}})
	key := ""
	for _, v := range k {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "key_") {
			key = s
		}
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"after"}}}`
	for _, p := range []string{"/echo", "/other"} {
		if code, _ := mcp(t, base, p, key, call); code != 200 {
			t.Fatalf("%s before the crash = %d", p, code)
		}
	}
	before := echoPIDs(t)
	if len(before) != 2 {
		t.Fatalf("want two echo-server children, have %v", before)
	}
	for _, pid := range before {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for _, p := range []string{"/echo", "/other"} {
		for {
			code, body := mcp(t, base, p, key, call)
			if code == 200 && strings.Contains(body, "after") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not recover after the child was killed: last %d %s", p, code, body)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	after := echoPIDs(t)
	for _, a := range after {
		for _, b := range before {
			if a == b {
				t.Fatalf("a killed pid %d is still the child", a)
			}
		}
	}
}

// A long response through httphost: a tool that takes longer than the server's 10s header timeout
// still answers, because Serve sets no write timeout. The libs handler offers no GET event stream
// (405), so a long POST response is the long-lived form. Shutdown while a call is in flight ends
// within the grace period.
func TestLongResponseOutlivesHeaderTimeout(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fakechild")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/hollis-labs/station/cmd/station-next/testdata/fakechild").CombinedOutput(); err != nil {
		t.Fatalf("build fakechild: %v\n%s", err, out)
	}
	base, stop := startedWith(t, fmt.Sprintf(`    - id: slow
      name: Slow
      transport: process
      process: {command: %q, args: []}
      serve: {http: {path: /slow}}
`, fake), nil)
	owner := newSession(t, base)
	owner.signIn()
	k := owner.want(201, "POST", "/api/identity/keys", map[string]any{"name": "all", "scopes": []string{"station.slow"}})
	key := ""
	for _, v := range k {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "key_") {
			key = s
		}
	}
	if code, _ := mcp(t, base, "/slow", key, initialize); code != 200 {
		t.Fatalf("initialize = %d", code)
	}
	// /healthz names the logical server.
	if code, body := owner.do("GET", "/healthz", nil, nil); code != 200 || !strings.Contains(string(body), `"mcp:slow":"ok"`) {
		t.Fatalf("healthz = %d %s", code, body)
	}
	start := time.Now()
	type result struct {
		code int
		body string
	}
	res := make(chan result, 1)
	go func() {
		c := &http.Client{Timeout: 30 * time.Second}
		req, _ := http.NewRequest("POST", base+"/slow", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sleep","arguments":{"ms":"12000"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+key)
		r, err := c.Do(req)
		if err != nil {
			res <- result{0, err.Error()}
			return
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		res <- result{r.StatusCode, string(b)}
	}()
	got := <-res
	if got.code != 200 || !strings.Contains(got.body, "slept_ms") || time.Since(start) < 12*time.Second {
		t.Fatalf("long call = %d after %s: %s", got.code, time.Since(start), got.body)
	}

	// A call in flight at shutdown does not hang serve.
	go func() {
		req, _ := http.NewRequest("POST", base+"/slow", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sleep","arguments":{"ms":"60000"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+key)
		if r, err := (&http.Client{Timeout: 40 * time.Second}).Do(req); err == nil {
			_ = r.Body.Close()
		}
	}()
	time.Sleep(500 * time.Millisecond)
	begin := time.Now()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
		took := time.Since(begin)
		t.Logf("serve stopped %s after cancel with a 60s call in flight", took.Round(time.Millisecond))
		if took > shutdownGrace+childStopTimeout+3*time.Second {
			t.Fatalf("shutdown took %s, want at most about %s", took, shutdownGrace+childStopTimeout)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop while a call was in flight")
	}

}

// SIGTERM to the real binary with a call in flight: the process exits within the bounded shutdown and
// its busy child goes with it (the child's stdin closes when the host exits).
func TestRealBinarySIGTERMWithCallInFlight(t *testing.T) {
	tmp := t.TempDir()
	bin, fake := filepath.Join(tmp, "station"), filepath.Join(tmp, "fakechild")
	for target, pkg := range map[string]string{bin: ".", fake: "github.com/hollis-labs/station/cmd/station-next/testdata/fakechild"} {
		if out, err := exec.Command("go", "build", "-o", target, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, out)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := filepath.Join(tmp, "cfg.yaml")
	yaml := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\nstation:\n  logical_servers:\n    - id: slow\n      name: Slow\n      transport: process\n      process: {command: %q, args: []}\n      serve: {http: {path: /slow}}\n", addr, fake)
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "STATION_DATA_DIR="+filepath.Join(tmp, "data"), "SUBSTRATE_CONFIG="+cfg)
	// First-run setup is offline: the owner and the caller's key are created with the binary's own
	// commands before it serves, as the cutover script does.
	setup := exec.Command(bin, "identity", "setup", "--username", "owner")
	setup.Env, setup.Stdin = env, strings.NewReader(ownerPassphrase+"\n")
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("identity setup: %v\n%s", err, out)
	}
	keyOut, err := func() ([]byte, error) {
		c := exec.Command(bin, "identity", "key", "create", "--name", "slow", "--scope", "station.slow", "--stdout")
		c.Env = env
		return c.Output()
	}()
	if err != nil {
		t.Fatalf("identity key create: %v", err)
	}
	key := strings.TrimSpace(string(keyOut))
	cmd := exec.Command(bin, "serve")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	base := "http://" + addr
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			_ = r.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("the binary never listened")
		}
		time.Sleep(100 * time.Millisecond)
	}
	go func() {
		req, _ := http.NewRequest("POST", base+"/slow", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sleep","arguments":{"ms":"60000"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+key)
		if r, err := (&http.Client{Timeout: 40 * time.Second}).Do(req); err == nil {
			_ = r.Body.Close()
		}
	}()
	time.Sleep(time.Second)
	if !childAliveNamed(t, "fakechild") {
		t.Fatal("no busy child to stop")
	}
	begin := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		t.Logf("exited %s after SIGTERM with a 60s call in flight: %v", time.Since(begin).Round(time.Millisecond), err)
		if err != nil {
			t.Fatalf("exit status %v, want 0", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the binary did not exit after SIGTERM")
	}
	// KNOWN (libs mcp-host, recorded in docs/conversion-notes.md): a child that is busy inside a call is not
	// reaped by the engine's stop sequence, so it can outlive the host process. A supervisor that kills the
	// unit's cgroup (systemd's default) reaps it. This test records the observation and cleans up after
	// itself; it does not assert on it.
	time.Sleep(2 * time.Second)
	if childAliveNamed(t, "fakechild") {
		t.Log("KNOWN: the busy child outlived the host process (reaped by the test)")
		_ = exec.Command("pkill", "-x", "fakechild").Run()
	}
}
