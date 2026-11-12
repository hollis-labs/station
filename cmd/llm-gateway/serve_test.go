package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	return gatewayDocAt(t, "http://127.0.0.1:9/v1", models...)
}

// gatewayDocAt is gatewayDoc with the one provider's base URL given.
func gatewayDocAt(t *testing.T, baseURL string, models ...string) string {
	t.Helper()
	if len(models) == 0 {
		models = []string{"chat"}
	}
	var ms []string
	for _, m := range models {
		ms = append(ms, fmt.Sprintf(`{"name":%q,"targets":[{"provider":"local","model":"upstream"}]}`, m))
	}
	return fmt.Sprintf(`{"schema_version":1,"gateway":{
 "providers":[{"id":"local","dialect":"openai","base_url":%q,"api_key_env":"LLMGW_TEST_KEY"}],
 "models":[%s],
 "catalog":{"refresh":false,"cache_dir":%q,"warmup":false}}}`, baseURL, strings.Join(ms, ","), filepath.Join(t.TempDir(), "catalog"))
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
			run, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, appboot.Options(os.Getenv, substrateModules, appboot.ModeCreate))
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
	// relay route without its scope, and the old client routes.
	for _, tc := range []struct {
		name, method, path, key string
		hdr                     map[string]string
		want                    []int
	}{
		{"no key", "GET", mgmt + "/status", "", nil, []int{401}},
		{"wrong key", "GET", mgmt + "/status", "not-a-key", nil, []int{401}},
		{"narrow scope", "GET", mgmt + "/status", narrow, nil, []int{403}},
		{"foreign host", "GET", mgmt + "/status", ops, map[string]string{"Host": "gateway.example.test"}, []int{403}},
		{"relay route without its scope", "POST", "/api/llm-gateway/v1/embeddings", narrow, nil, []int{403}},
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

// The live clients reach the gateway by its public name through a local TLS proxy. A listed Host is
// served only from a loopback peer carrying exactly one X-Forwarded-Proto: https; anything else is
// refused, and the key's scopes still decide what the request may do.
func TestServedThroughTheTLSProxyName(t *testing.T) {
	const public = "gw.example.test"
	u := newUnit(t, "")
	cfg := fmt.Sprintf("httphost: {addr: %q}\nidentity: {bcrypt_cost: 4, trusted_proxy: true}\nllm-gateway: {allowed_hosts: [%s]}\n", u.addr, public)
	if err := os.WriteFile(u.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	ops := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "ops", "--scope", "llm-gateway.management.read", "--stdout"))
	narrow := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "chat-only", "--scope", "llm-gateway.chat", "--stdout"))
	collector := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "usage-collector", "--scope", "llm-gateway.usage.read", "--stdout"))
	u.mustRunStdout("", "gateway-store", "stage", "--input", writeFile(t, "doc.json", gatewayDoc(t)), "--expected", "0", "--actor", "test")
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")

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
	status := "/api/llm-gateway/v1/management/status"
	var callPath func(path, key, host string, proto ...string) int
	call := func(key, host string, proto ...string) int { return callPath(status, key, host, proto...) }
	callPath = func(path, key, host string, proto ...string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Host = host
		for _, p := range proto {
			req.Header.Add("X-Forwarded-Proto", p)
		}
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, tc := range []struct {
		name  string
		key   string
		host  string
		proto []string
		want  int
	}{
		{"proxied, listed host, one https", ops, public, []string{"https"}, 200},
		{"listed host without the proxy header", ops, public, nil, 403},
		{"listed host, http claimed", ops, public, []string{"http"}, 403},
		{"listed host, two proxy values", ops, public, []string{"https", "https"}, 403},
		{"unlisted host with the header", ops, "other.example.test", []string{"https"}, 403},
		{"proxied, key without the scope", narrow, public, []string{"https"}, 403},
		{"loopback host, no proxy", ops, u.addr, nil, 200},
	} {
		if got := call(tc.key, tc.host, tc.proto...); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// The usage monitor's collectors arrive the same way, with the key that holds only llm-gateway.usage.read.
	export := "/api/llm-gateway/v1/usage/export"
	for _, tc := range []struct {
		name  string
		key   string
		proto []string
		want  int
	}{
		{"collector key through the proxy name", collector, []string{"https"}, 200},
		{"collector key, proxy header missing", collector, nil, 403},
		{"chat-only key on the usage export", narrow, []string{"https"}, 403},
	} {
		if got := callPath(export, tc.key, public, tc.proto...); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := callPath("/api/llm-gateway/v1/management/status", collector, public, "https"); got != 403 {
		t.Errorf("the collector key on a management route: %d, want 403", got)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit status %v\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the binary did not exit after SIGTERM")
	}
}

// The live units carry HOLLIS_AUTH_STATION=permissive (a dev posture dial added to the old build's units).
// The unit's own auth.mode is enforce and the module reads the environment only when the config sets no
// mode, so the variable, and its general and per-app forms, must change nothing: no credential, no entry.
// The unit also runs from an empty working directory and must leave it empty.
func TestAmbientAuthPostureDoesNotWeakenEnforcement(t *testing.T) {
	u := newUnit(t, "")
	u.env = append(u.env, "HOLLIS_AUTH_STATION=permissive", "HOLLIS_AUTH_LLM_GATEWAY=permissive", "HOLLIS_AUTH=off")
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	ops := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "ops", "--scope", "llm-gateway.management.read", "--stdout"))
	u.mustRunStdout("", "gateway-store", "stage", "--input", writeFile(t, "doc.json", gatewayDoc(t)), "--expected", "0", "--actor", "test")
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")

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
	call := func(key string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/api/llm-gateway/v1/management/status", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"no credential", "", 401},
		{"wrong credential", "not-a-key", 401},
		{"valid credential (control: the server answers)", ops, 200},
	} {
		if got := call(tc.key); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// The guard reports the mode it runs in: the configured one, whatever the environment says.
	req, _ := http.NewRequest("GET", base+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+ops)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	meBody, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(meBody), `"mode":"enforce"`) {
		t.Errorf("/api/me = %d %s, want mode enforce", res.StatusCode, meBody)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit status %v\n%s", err, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the binary did not exit after SIGTERM")
	}
	if left, _ := os.ReadDir(u.work); len(left) != 0 {
		t.Errorf("the working directory is not empty after a run: %d entries", len(left))
	}
}

// startedGateway runs the real binary on a throwaway data directory with one active revision whose only
// provider is upstream, and returns the base URL. The caller stops nothing: the cleanup kills the process.
func (u *unit) startedGateway(upstream string) string {
	u.t.Helper()
	u.mustRun(ownerPassphrase+"\n", "identity", "setup", "--username", "owner")
	u.mustRunStdout("", "gateway-store", "stage", "--input", writeFile(u.t, "doc.json", gatewayDocAt(u.t, upstream)), "--expected", "0", "--actor", "test")
	u.mustRun("", "gateway-store", "activate", "--revision", "1", "--expected", "0", "--actor", "test")
	cmd := u.command("", "serve")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		u.t.Fatal(err)
	}
	u.t.Cleanup(func() { _ = cmd.Process.Kill() })
	base := "http://" + u.addr
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			_ = r.Body.Close()
			return base
		}
		if i > 100 {
			u.t.Fatalf("the binary never listened\n%s", stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The relay's clients depend on the upstream dialect's error bodies. The framework rewrites error bodies into its
// flat {code, message} shape unless the path is declared raw, so this runs the real binary: an upstream
// OpenAI-style error must come back byte for byte, the gateway's own relay errors keep their OpenAI shape, and
// the framework's own refusals on the same paths (no key, no scope, unmapped route) stay flat.
func TestRelayErrorsReachTheClientUntouchedAndFrameworkRefusalsStayFlat(t *testing.T) {
	// Not canonical JSON on purpose (spacing, key order, a trailing newline): a decode and re-encode would change it.
	const upstreamBody = "{\"error\": {\"message\": \"Unsupported  parameter: 'x'\", \"type\": \"invalid_request_error\", \"param\": \"x\", \"code\": \"unsupported_parameter\"}}\n"
	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "kept")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer up.Close()

	u := newUnit(t, "")
	base := u.startedGateway(up.URL + "/v1")
	chat := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "chat", "--scope", "llm-gateway.chat", "--stdout"))
	ops := strings.TrimSpace(u.mustRunStdout("", "identity", "key", "create", "--name", "ops", "--scope", "llm-gateway.management.read", "--stdout"))

	do := func(method, path, key, body string) (int, http.Header, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, string(b)
	}
	relay := "/api/llm-gateway/v1/chat/completions"
	req := `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`

	// An upstream error arrives untouched: status, body bytes and headers; the caller's key was not forwarded.
	code, hdr, body := do("POST", relay, chat, req)
	if code != 400 || body != upstreamBody || hdr.Get("X-Upstream-Marker") != "kept" {
		t.Errorf("upstream error = %d %q (marker %q), want 400 and the upstream bytes", code, body, hdr.Get("X-Upstream-Marker"))
	}
	if sawAuth != "Bearer not-a-real-upstream-key" {
		t.Errorf("the upstream saw Authorization %q: the caller's key must be replaced by the provider key", sawAuth)
	}
	// The gateway's own relay error keeps the OpenAI shape (an unknown model).
	code, _, body = do("POST", relay, chat, `{"model":"nope","messages":[]}`)
	var own struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &own); code != 404 && code != 400 || err != nil || own.Error.Message == "" {
		t.Errorf("gateway-own relay error = %d %s, want an OpenAI-shaped {\"error\":{...}}", code, body)
	}

	// The framework's refusals on the same paths are flat: {code, message} and no nested error.
	flat := func(name string, code int, body string, want int, wantCode string) {
		t.Helper()
		var f map[string]any
		if err := json.Unmarshal([]byte(body), &f); err != nil || code != want {
			t.Errorf("%s = %d %s, want %d flat", name, code, body, want)
			return
		}
		if _, nested := f["error"]; nested || f["code"] == nil || f["message"] == nil || (wantCode != "" && f["code"] != wantCode) {
			t.Errorf("%s = %s, want a flat {code, message} (code %q)", name, body, wantCode)
		}
	}
	c, _, b := do("POST", relay, "", req)
	flat("no key on a relay path", c, b, 401, "")
	c, _, b = do("POST", relay, ops, req)
	flat("a key without llm-gateway.chat", c, b, 403, "")
	c, _, b = do("GET", "/api/llm-gateway/v1/nosuch", chat, "")
	flat("an unmapped path under the relay prefix", c, b, 403, "route_not_mapped")
}
