package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/module"
)

var (
	echoBin   string
	guardRoot string // scratch HOME for the whole package (see TestMain)
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "station-echo")
	if err != nil {
		panic(err)
	}
	echoBin = filepath.Join(dir, "echo-server")
	if out, err := exec.Command("go", "build", "-o", echoBin, "github.com/hollis-labs/station/plugins/echo-server").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build echo-server: %v\n%s", err, out)
		os.Exit(2)
	}
	// A run without STATION_DATA_DIR resolves the XDG data home of app `station`, which on a
	// real machine is a live directory. Point every location at a scratch root for the whole
	// package, so a test (or a command it starts) that forgets the variable cannot reach it;
	// TestDefaultDataDirIsNotLive proves it.
	guardRoot = filepath.Join(dir, "home")
	for k, v := range map[string]string{"HOME": guardRoot, "XDG_DATA_HOME": filepath.Join(guardRoot, "data"),
		"XDG_STATE_HOME": filepath.Join(guardRoot, "state"), "XDG_CONFIG_HOME": filepath.Join(guardRoot, "config")} {
		_ = os.Setenv(k, v)
	}
	_ = os.Unsetenv("STATION_DATA_DIR")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) { e.mu.Lock(); e.list = append(e.list, s); e.mu.Unlock() }
func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

// started boots the real stack (every module the manifest selects, the real handler chain) on a
// temp data directory and a free loopback port, with two echo-server children, and returns the
// base URL and a stop function that returns the shutdown events once serve has ended.
func started(t *testing.T) (string, func() []string) {
	t.Helper()
	return startedWith(t, fmt.Sprintf(`    - id: echo
      name: Echo
      transport: process
      process: {command: %q, args: []}
      serve: {http: {path: /echo}}
    - id: other
      name: Other
      transport: process
      process: {command: %q, args: []}
      serve: {http: {path: /other}}
`, echoBin, echoBin), nil)
}

// startedWith is started with the logical_servers YAML (indented four spaces under the list) given by
// the caller. provision, when set, runs after the modules start and before serve, so a test can put
// stand-in values in the secrets store that the config refers to.
func startedWith(t *testing.T, servers string, provision func(context.Context, *module.App)) (string, func() []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	override := fmt.Sprintf(`httphost: {addr: "127.0.0.1:0"}
identity: {bcrypt_cost: 4}
station:
  logical_servers:
%s`, servers)
	file := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(file, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	// Every test uses an explicit temporary data directory: the default one is the live one.
	t.Setenv("STATION_DATA_DIR", dir)
	t.Setenv("SUBSTRATE_CONFIG", file)

	ctx, cancel := context.WithCancel(context.Background())
	run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, boot.Options{AfterConfig: afterConfig(os.Getenv, true)})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if provision != nil {
		provision(ctx, run.App)
	}
	ev := &events{}
	addr := make(chan net.Addr, 1)
	s := &station{app: run.App, ready: func(a net.Addr) { addr <- a }, event: ev.add}
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx) }()
	stopped := false
	stop := func() []string {
		if stopped {
			return ev.get()
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve ended with %v", err)
			}
		case <-time.After(45 * time.Second):
			t.Error("serve did not stop on cancel")
		}
		ev.add("modules-stopping")
		_ = run.Stop(context.Background())
		return ev.get()
	}
	t.Cleanup(func() { stop() })
	select {
	case a := <-addr:
		return "http://" + a.String(), stop
	case err := <-done:
		t.Fatalf("serve returned before listening: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("serve never listened")
	}
	return "", stop
}

type session struct {
	t    *testing.T
	base string
	http *http.Client
}

func newSession(t *testing.T, base string) *session {
	jar, _ := cookiejar.New(nil)
	return &session{t: t, base: base, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

func (s *session) do(method, path string, body any, headers map[string]string) (int, []byte) {
	s.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rdr)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "station-test")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := s.http.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (s *session) want(status int, method, path string, body any) map[string]any {
	s.t.Helper()
	got, out := s.do(method, path, body, nil)
	if got != status {
		s.t.Fatalf("%s %s: %d want %d: %s", method, path, got, status, out)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// mcp posts an MCP request to path with key as the bearer ("" for none) and returns the status.
func mcp(t *testing.T, base, path, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func childAlive(t *testing.T) bool {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", echoBin).Output()
	return strings.TrimSpace(string(out)) != ""
}

func TestAcceptance(t *testing.T) {
	base, stop := started(t)

	// The host is up; health is open; the engine's two children run.
	anon := newSession(t, base)
	anon.want(200, "GET", "/healthz", nil)
	if !childAlive(t) {
		t.Fatal("the logical-server children are not running")
	}
	// Mounts are closed to an anonymous caller, and the identity entry points are open.
	if code, _ := mcp(t, base, "/echo", "", initialize); code != 401 {
		t.Fatalf("anonymous /echo = %d, want 401", code)
	}
	if st := anon.want(200, "GET", "/api/setup", nil); st["setup_required"] != true {
		t.Fatalf("first run should require setup: %v", st)
	}

	// First-run setup creates the owner; the owner issues keys (shown once).
	owner := newSession(t, base)
	owner.want(200, "POST", "/api/setup", map[string]any{"display_name": "Owner", "username": "owner", "password": "a long test passphrase"})
	issue := func(name string, scopes ...string) string {
		k := owner.want(201, "POST", "/api/identity/keys", map[string]any{"name": name, "scopes": scopes})
		for _, f := range []string{"key", "secret", "token"} {
			if v, ok := k[f].(string); ok && strings.HasPrefix(v, "key_") {
				return v
			}
		}
		t.Fatalf("no key in the response (fields: %v)", keys(k))
		return ""
	}
	echoKey := issue("echo-caller", "station.echo")
	otherKey := issue("other-caller", "station.other")
	wildKey := issue("wild-caller", "station.*")
	noScope := issue("wrong-scope", "llm-gateway.chat")

	// 200 with the right key; a key for one server is refused on the other; wildcard covers both.
	if code, body := mcp(t, base, "/echo", echoKey, initialize); code != 200 || !strings.Contains(body, "protocolVersion") {
		t.Fatalf("echo key on /echo = %d %s", code, body)
	}
	if code, _ := mcp(t, base, "/other", echoKey, initialize); code != 403 {
		t.Fatalf("echo key on /other = %d, want 403", code)
	}
	if code, _ := mcp(t, base, "/echo", otherKey, initialize); code != 403 {
		t.Fatalf("other key on /echo = %d, want 403", code)
	}
	for _, p := range []string{"/echo", "/other"} {
		if code, _ := mcp(t, base, p, wildKey, initialize); code != 200 {
			t.Fatalf("station.* key on %s = %d, want 200", p, code)
		}
	}
	if code, _ := mcp(t, base, "/echo", noScope, initialize); code != 403 {
		t.Fatalf("key with another app's scope on /echo = %d, want 403", code)
	}
	if code, _ := mcp(t, base, "/echo", "key_not-a-real-key", initialize); code != 401 {
		t.Fatalf("unknown key = %d, want 401", code)
	}
	// A tool call end to end through identity, the guard and the engine.
	if code, body := mcp(t, base, "/echo", echoKey, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hi"}}}`); code != 200 || !strings.Contains(body, `echo`) {
		t.Fatalf("tools/call = %d %s", code, body)
	}
	// A request for a Host the service is not meant to have is refused.
	if code, _ := anon.do("GET", "/healthz", nil, map[string]string{"Host": "evil.example.test"}); code != 403 {
		t.Fatalf("unknown Host = %d, want 403", code)
	}

	// Shutdown order: HTTP drains, then the logical servers stop, then the modules.
	got := stop()
	want := []string{"http-drained", "logical-servers-stopped", "modules-stopping"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if childAlive(t) {
		t.Fatal("a logical-server child outlived serve")
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
