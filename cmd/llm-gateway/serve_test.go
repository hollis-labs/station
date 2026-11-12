package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/boot"

	"github.com/hollis-labs/station/internal/appboot"
)

const ownerPassphrase = "a long test passphrase"

// gatewayDoc is a minimal valid revision: one OpenAI-dialect provider (its key from the
// environment) and one model, with the catalog kept off the network.
func gatewayDoc(t *testing.T, models ...string) string {
	t.Helper()
	if len(models) == 0 {
		models = []string{"chat"}
	}
	var ms []string
	for _, m := range models {
		ms = append(ms, fmt.Sprintf(`{"name":%q,"targets":[{"provider":"local","model":"upstream"}]}`, m))
	}
	return fmt.Sprintf(`{"schema_version":1,"gateway":{
 "providers":[{"id":"local","dialect":"openai","base_url":"http://127.0.0.1:9/v1","api_key_env":"LLMGW_TEST_KEY"}],
 "models":[%s],
 "catalog":{"refresh":false,"cache_dir":%q,"warmup":false}}}`, strings.Join(ms, ","), filepath.Join(t.TempDir(), "catalog"))
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The framework's bind check accepts every address in enforce mode, so refusing a non-loopback
// httphost.addr is Station's rule. It must hold before anything listens or the gateway starts.
func TestServeRefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.5:0", "100.64." + "0.5:0"} {
		t.Run(addr, func(t *testing.T) {
			u := newUnit(t, "")
			override := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4}\n", addr)
			if err := os.WriteFile(u.cfg, []byte(override), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("LLM_GATEWAY_DATA_DIR", u.dir)
			t.Setenv("SUBSTRATE_CONFIG", u.cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, appboot.Options(os.Getenv, substrateModules, true))
			if err != nil {
				t.Fatal(err)
			}
			defer run.Stop(context.Background()) //nolint:errcheck
			listened := false
			g := &gateway{app: run.App, ready: func(net.Addr) { listened = true }}
			err = g.serve(ctx)
			if err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("serve error = %v, want a loopback refusal", err)
			}
			if listened {
				t.Fatal("serve listened before refusing the address")
			}
		})
	}
}

// First run, then the real binary: offline owner and keys, a staged and activated revision, serve,
// a write and a read through the management API, refused requests, and SIGTERM.
func TestRealBinaryLifecycle(t *testing.T) {
	u := newUnit(t, "")

	// Serving before any revision exists says what to do and listens on nothing.
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	if out, err := u.run("", "serve"); err == nil || !strings.Contains(out, "no active gateway revision") {
		t.Fatalf("serve with no revision: %v\n%s", err, out)
	}

	ops := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "ops", "--scope", "llm-gateway.management.read", "--scope", "llm-gateway.management.write", "--stdout"))
	narrow := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "chat-only", "--scope", "llm-gateway.chat", "--stdout"))

	doc := writeFile(t, "doc.json", gatewayDoc(t))
	staged := u.mustRunStdout("", "gateway-store", "stage", "--input", doc, "--expected", "0", "--actor", "test")
	if !strings.Contains(staged, `"id":1`) {
		t.Fatalf("stage output: %s", staged)
	}
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")
	if out := u.mustRunStdout("", "validate"); !strings.Contains(out, "OK, revision 1") {
		t.Fatalf("validate: %s", out)
	}

	cmd := u.command("", "serve")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	base := "http://" + u.addr
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			_ = r.Body.Close()
			break
		}
		if i > 100 {
			t.Fatalf("the binary never listened\n%s", stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	call := func(method, path, key, body string, hdr map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
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
	mgmt := "/api/llm-gateway/v1/management"

	// Read: the running revision.
	code, body := call("GET", mgmt+"/status", ops, "", nil)
	var status struct {
		RunningRevision int64 `json:"running_revision"`
	}
	if err := json.Unmarshal([]byte(body), &status); code != 200 || err != nil || status.RunningRevision != 1 {
		t.Fatalf("status = %d %s", code, body)
	}
	// Write: stage a second revision through the API, with the precondition, then read it back.
	next := gatewayDoc(t, "chat", "second")
	code, body = call("PUT", mgmt+"/config", ops, next, map[string]string{"If-Match": `"1"`, "Content-Type": "application/json"})
	if code != 201 || !strings.Contains(body, `"revision":2`) {
		t.Fatalf("stage revision 2 = %d %s", code, body)
	}
	code, body = call("GET", mgmt+"/models?revision=2", ops, "", nil)
	if code != 200 || !strings.Contains(body, `"second"`) {
		t.Fatalf("read revision 2 = %d %s", code, body)
	}

	// Refused: no key, a key without the scope, a wrong key, a Host the unit does not serve, a
	// relay route (not mounted), and the old client routes.
	for _, tc := range []struct {
		name, method, path, key string
		hdr                     map[string]string
		want                    []int
	}{
		{"no key", "GET", mgmt + "/status", "", nil, []int{401}},
		{"wrong key", "GET", mgmt + "/status", "not-a-key", nil, []int{401}},
		{"narrow scope", "GET", mgmt + "/status", narrow, nil, []int{403}},
		{"foreign host", "GET", mgmt + "/status", ops, map[string]string{"Host": "gateway.example.test"}, []int{403}},
		{"relay route", "POST", "/api/llm-gateway/v1/chat/completions", narrow, nil, []int{403}},
		{"clients route", "GET", mgmt + "/clients", ops, nil, []int{403}},
	} {
		got, out := call(tc.method, tc.path, tc.key, `{}`, tc.hdr)
		t.Logf("%s: %d %s", tc.name, got, strings.TrimSpace(out))
		ok := false
		for _, w := range tc.want {
			ok = ok || got == w
		}
		if !ok {
			t.Errorf("%s: %d %s, want one of %v", tc.name, got, out, tc.want)
		}
	}

	begin := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		t.Logf("exited %s after SIGTERM: %v", time.Since(begin).Round(time.Millisecond), err)
		if err != nil {
			t.Fatalf("exit status %v, want 0\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the binary did not exit after SIGTERM")
	}
	for _, secret := range []string{ops, narrow, "not-a-real-upstream-key", ownerPassphrase} {
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("the log contains a credential")
		}
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
