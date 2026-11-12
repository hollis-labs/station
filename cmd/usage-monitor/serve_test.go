package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/station/internal/appboot"
)

const ownerPassphrase = "a long test passphrase"

func writeConfig(t *testing.T, u *unit, body string) {
	t.Helper()
	text := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\n%s", u.addr, body)
	if err := os.WriteFile(u.cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (u *unit) mustRunStdout(stdin string, args ...string) string {
	u.t.Helper()
	c := u.command(stdin, args...)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		u.t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

// served is a running `usage-monitor serve` process.
type served struct {
	cmd    *exec.Cmd
	exited chan error
	stderr *strings.Builder
	base   string
}

func (u *unit) serve() *served {
	u.t.Helper()
	s := &served{stderr: &strings.Builder{}, exited: make(chan error, 1), base: "http://" + u.addr}
	s.cmd = u.command("", "serve")
	s.cmd.Stderr = s.stderr
	if err := s.cmd.Start(); err != nil {
		u.t.Fatal(err)
	}
	go func() { s.exited <- s.cmd.Wait() }()
	u.t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	for i := 0; ; i++ {
		if r, err := http.Get(s.base + "/healthz"); err == nil {
			_ = r.Body.Close()
			return s
		}
		if i > 100 {
			u.t.Fatalf("the binary never listened\n%s", s.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop sends SIGTERM and requires a clean exit.
func (s *served) stop(t *testing.T) {
	t.Helper()
	begin := time.Now()
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-s.exited:
		t.Logf("exited %s after SIGTERM: %v", time.Since(begin).Round(time.Millisecond), err)
		if err != nil {
			t.Fatalf("exit status %v, want 0\n%s", err, s.stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the binary did not exit after SIGTERM")
	}
}

func (s *served) call(t *testing.T, method, path, key, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, s.base+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The framework's bind check accepts every address in enforce mode, so refusing a non-loopback
// httphost.addr is Station's rule. It must hold before anything listens or a collector starts.
func TestServeRefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.5:0", "100.64." + "0.5:0"} {
		t.Run(addr, func(t *testing.T) {
			u := newUnit(t, "")
			override := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\n", addr)
			if err := os.WriteFile(u.cfg, []byte(override), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("USAGE_MONITOR_DATA_DIR", u.dir)
			t.Setenv("SUBSTRATE_CONFIG", u.cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, appboot.Options(os.Getenv, substrateModules, true))
			if err != nil {
				t.Fatal(err)
			}
			defer run.Stop(context.Background()) //nolint:errcheck
			listened := false
			m := &monitor{app: run.App, ready: func(net.Addr) { listened = true }}
			err = m.serve(ctx)
			if err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("serve error = %v, want a loopback refusal", err)
			}
			if listened {
				t.Fatal("serve listened before refusing the address")
			}
		})
	}
}

const bootstrapAccounts = `usage-monitor:
  accounts:
    - {id: claude, provider: anthropic, kind: subscription, label: Claude, enabled: true}
`

// First run, then the real binary: offline owner and keys, serve, a read and a write through the API,
// a push source bound to an identity-issued key (its principal is key:<id>), refused requests, and
// SIGTERM, twice (the second start adds the push source once the key id is known).
func TestRealBinaryLifecycle(t *testing.T) {
	u := newUnit(t, "")
	writeConfig(t, u, bootstrapAccounts)
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	key := func(name string, scopes ...string) string {
		args := []string{"identity", "key", "create", "--name", name, "--stdout"}
		for _, s := range scopes {
			args = append(args, "--scope", s)
		}
		return strings.TrimSpace(u.mustRunStdout("", args...))
	}
	operator := key("operator", "usage-monitor.read", "usage-monitor.manage")
	reader := key("reader", "usage-monitor.read")
	reporter := key("reporter", "usage-monitor.ingest")
	intruder := key("intruder", "usage-monitor.ingest") // holds the scope, but is not the source's principal
	admin := key("admin", "usage-monitor.admin")

	s := u.serve()
	// The id of the reporter's key (the principal of a push source is key:<id>), from the identity API.
	code, body := s.call(t, "GET", "/api/identity/keys", admin, "", nil)
	var listed struct {
		Keys []struct{ ID, Name string } `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &listed); code != 200 || err != nil {
		t.Fatalf("list keys = %d %s", code, body)
	}
	var reporterID string
	for _, k := range listed.Keys {
		if k.Name == "reporter" {
			reporterID = k.ID
		}
	}
	if reporterID == "" {
		t.Fatalf("no reporter key in %s", body)
	}
	s.stop(t)

	writeConfig(t, u, bootstrapAccounts+fmt.Sprintf("  sources:\n    - {id: laptop, account_id: claude, collector: push, principal: %q}\n", "key:"+reporterID))
	s = u.serve()
	api := "/api/usage-monitor/v1"

	// Read: the bootstrap account.
	code, body = s.call(t, "GET", api+"/accounts", reader, "", nil)
	if code != 200 || !strings.Contains(body, `"id":"claude"`) {
		t.Fatalf("accounts = %d %s", code, body)
	}
	// Write: a push batch as the bound key, then read it back with the actor the principal gives.
	batch := fmt.Sprintf(`{"observations":[{"id":"one","observed_at":%q,"status":"ok","windows":[{"id":"five_hour","unit":"percent","used":25,"limit":100,"remaining":75,"resets_at":null}]}]}`, time.Now().UTC().Format(time.RFC3339Nano))
	if code, body = s.call(t, "POST", api+"/sources/laptop/ingest", reporter, batch, nil); code != 200 {
		t.Fatalf("ingest = %d %s", code, body)
	}
	if code, body = s.call(t, "GET", api+"/observations", reader, "", nil); code != 200 || !strings.Contains(body, `"actor":"key:`+reporterID+`"`) {
		t.Fatalf("observations = %d %s", code, body)
	}
	// Write: edit the account with the revision precondition; a stale revision is a conflict.
	update := `{"account":{"id":"claude","provider":"anthropic","kind":"subscription","label":"paused","enabled":false},"expected_revision":1}`
	if code, body = s.call(t, "PUT", api+"/accounts/claude", operator, update, nil); code != 200 {
		t.Fatalf("update account = %d %s", code, body)
	}
	if code, _ = s.call(t, "PUT", api+"/accounts/claude", operator, update, nil); code != 409 {
		t.Fatalf("stale update = %d, want 409", code)
	}

	// Refused.
	for _, tc := range []struct {
		name, method, path, key, body string
		hdr                           map[string]string
		want                          int
	}{
		{"no key", "GET", api + "/accounts", "", "", nil, 401},
		{"wrong key", "GET", api + "/accounts", "not-a-key", "", nil, 401},
		{"reader cannot manage", "PUT", api + "/accounts/claude", reader, update, nil, 403},
		{"reader cannot ingest", "POST", api + "/sources/laptop/ingest", reader, batch, nil, 403},
		{"another ingest key is not the source's principal", "POST", api + "/sources/laptop/ingest", intruder, batch, nil, 403},
		{"foreign host", "GET", api + "/accounts", reader, "", map[string]string{"Host": "usage.example.test"}, 403},
		{"unmapped route", "GET", api + "/v1/nothing", reader, "", nil, 403},
	} {
		got, out := s.call(t, tc.method, tc.path, tc.key, tc.body, tc.hdr)
		t.Logf("%s: %d %s", tc.name, got, strings.TrimSpace(out))
		if got != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, got, out, tc.want)
		}
		if strings.HasPrefix(tc.name, "another ingest key") && !strings.Contains(out, "source_forbidden") {
			t.Errorf("%s: %s, want source_forbidden", tc.name, out)
		}
	}
	s.stop(t)
	for _, secret := range []string{operator, reader, reporter, intruder, admin, ownerPassphrase} {
		if strings.Contains(s.stderr.String(), secret) {
			t.Errorf("the log contains a credential")
		}
	}
}
