package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type bodyPlugin struct{}

func (bodyPlugin) Name() string { return "body" }
func (bodyPlugin) Register(_ context.Context, reg *PluginRegistry, _ ConfigDecoder) error {
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		_, _ = io.WriteString(w, "read "+string(rune('0'+n%10)))
	})
	if err := reg.Mount(Route{Method: "POST", Path: "/small", Scope: "body.write", Handler: read}); err != nil {
		return err
	}
	return reg.Mount(Route{Method: "POST", Path: "/big", Scope: "body.write", MaxBodyBytes: 1 << 20, Handler: read})
}

func TestPerRouteBodyLimit(t *testing.T) {
	cfg, _ := ParseConfig([]byte("auth: {mode: off}\nmax_body_bytes: 1024\nplugins:\n  body: {}\n"))
	app, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet, Plugins: []Plugin{bodyPlugin{}}})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, n int) int {
		req := httptest.NewRequest("POST", path, bytes.NewReader(make([]byte, n)))
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post("/api/body/small", 2000); c != 413 {
		t.Fatalf("default limit not applied: %d", c)
	}
	if c := post("/api/body/small", 500); c != 200 {
		t.Fatalf("under the default: %d", c)
	}
	if c := post("/api/body/big", 2000); c != 200 {
		t.Fatalf("route override not applied: %d", c)
	}
	if c := post("/api/body/big", 2<<20); c != 413 {
		t.Fatalf("route cap not enforced: %d", c)
	}
}

// --- JWT -------------------------------------------------------------------

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(t *testing.T, priv ed25519.PrivateKey, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "k"})
	pl, _ := json.Marshal(claims)
	signing := b64(hdr) + "." + b64(pl)
	return signing + "." + b64(ed25519.Sign(priv, []byte(signing)))
}

func TestJWTAuthAlongsideAPIKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyFile := filepath.Join(t.TempDir(), "issuer.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	app := mustAssemble(t, `
auth:
  jwt: {issuer: tether, audience: station, bootstrap_key_file: `+keyFile+`}
  keys:
    - {id: k1, sha256: `+hashOf("secret")+`, scopes: ["demo.read"]}
plugins:
  demo: {}
`, &testPlugin{name: "demo", scope: "demo.read"})
	claims := func(mod func(map[string]any)) string {
		c := map[string]any{"iss": "tether", "aud": "station", "sub": "device-7", "scope": "demo.read", "exp": time.Now().Add(time.Hour).Unix()}
		if mod != nil {
			mod(c)
		}
		return signJWT(t, priv, c)
	}
	if rec := do(app, "GET", "/api/demo/ping", claims(nil)); rec.Code != 200 {
		t.Fatalf("valid jwt: %d", rec.Code)
	}
	for name, tok := range map[string]string{
		"expired":       claims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"wrong aud":     claims(func(c map[string]any) { c["aud"] = "other" }),
		"wrong iss":     claims(func(c map[string]any) { c["iss"] = "evil" }),
		"missing scope": claims(func(c map[string]any) { c["scope"] = "other.read" }),
	} {
		want := 401
		if name == "missing scope" {
			want = 403
		}
		if rec := do(app, "GET", "/api/demo/ping", tok); rec.Code != want {
			t.Errorf("%s: %d, want %d", name, rec.Code, want)
		}
	}
	// Signed by a different key.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged := signJWT(t, other, map[string]any{"iss": "tether", "aud": "station", "sub": "x", "scope": "demo.read", "exp": time.Now().Add(time.Hour).Unix()})
	if rec := do(app, "GET", "/api/demo/ping", forged); rec.Code != 401 {
		t.Fatalf("forged jwt: %d", rec.Code)
	}
	if rec := do(app, "GET", "/api/demo/ping", "secret"); rec.Code != 200 {
		t.Fatalf("static key must still work: %d", rec.Code)
	}
}

func TestJWTConfigNeedsAudience(t *testing.T) {
	cfg, _ := ParseConfig([]byte("auth:\n  jwt: {jwks_url: http://x/jwks.json}\n"))
	if _, err := Assemble(context.Background(), Options{Config: cfg, Logger: quiet}); err == nil {
		t.Fatal("jwt without audience accepted")
	}
}

// --- logging ---------------------------------------------------------------

func TestAccessLogCarriesPrincipalAndRouteButNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	cfg, _ := ParseConfig([]byte(authYAML("demo.read")))
	app, err := Assemble(context.Background(), Options{Config: cfg, Logger: logger, Plugins: []Plugin{&testPlugin{name: "demo", scope: "demo.read"}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/demo/ping?api_key=QUERYSECRET123", nil)
	req.Header.Set("Authorization", "Bearer secret")
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)

	var access map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "http" {
			access = m
		}
	}
	if access == nil {
		t.Fatalf("no access log line in %s", buf.String())
	}
	if access["principal"] != "k1" || access["verified"] != true || access["route"] != "GET /api/demo/ping" || access["status"] != float64(200) {
		t.Fatalf("access log %v", access)
	}
	if id, _ := access["request_id"].(string); len(id) != 16 {
		t.Fatalf("request_id %v", access["request_id"])
	}
	if strings.Contains(buf.String(), "QUERYSECRET123") || strings.Contains(buf.String(), "Bearer secret") {
		t.Fatalf("a credential reached the log: %s", buf.String())
	}
}

func TestDeniedRequestIsLogged(t *testing.T) {
	var buf bytes.Buffer
	cfg, _ := ParseConfig([]byte(authYAML("demo.read")))
	app, _ := Assemble(context.Background(), Options{Config: cfg, Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		Plugins: []Plugin{&testPlugin{name: "demo", scope: "demo.read"}}})
	app.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/demo/ping", nil))
	out := buf.String()
	if !strings.Contains(out, `"msg":"auth"`) || !strings.Contains(out, `"denied":true`) || !strings.Contains(out, `"status":401`) {
		t.Fatalf("a refused request must leave an auth event and an access line: %s", out)
	}
}

func TestLoggingRedactsKnownAndShapedSecrets(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogging(&buf, "json", slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	const known = "kN0wn-s3cret-VALUE-9f8e7d6c5b4a"
	l.Sink().Add(known)
	l.Logger.Info("resolved "+known, "detail", "key="+known, "header", "Authorization: Bearer abcdef0123456789abcdef0123456789", "api_key", "short")
	out := buf.String()
	for _, leaked := range []string{known, "abcdef0123456789abcdef0123456789", `"short"`} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%q leaked: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("nothing was redacted: %s", out)
	}
}
