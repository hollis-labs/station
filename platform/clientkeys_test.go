package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestClientFileAuthAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.yaml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf("- {id: app, sha256: %s, scopes: [demo.read]}\n", hashOf("app-token")))
	cfg := fmt.Sprintf("auth: {keys_file: %q}\nplugins: {demo: {}}", path)
	a := mustAssemble(t, cfg, &testPlugin{name: "demo", scope: "demo.read"})
	defer a.Close(context.Background())
	if do(a, "GET", "/api/demo/ping", "app-token").Code != 200 {
		t.Fatal("key file not accepted")
	}
	if do(a, "GET", "/api/demo/ping", "wrong").Code != 401 {
		t.Fatal("invalid key accepted")
	}
	write("[]\n")
	b := mustAssemble(t, cfg, &testPlugin{name: "demo", scope: "demo.read"})
	defer b.Close(context.Background())
	if do(b, "GET", "/api/demo/ping", "app-token").Code != 401 {
		t.Fatal("revoked key accepted on reload")
	}
	write(fmt.Sprintf("- {id: app, sha256: %s, scopes: [demo.read], typo: true}\n", hashOf("app-token")))
	if _, err := LoadClientKeys(path); err == nil {
		t.Fatal("unknown key field accepted")
	}
}
