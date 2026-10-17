package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/station/platform"
)

func TestClientLifecycle(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "clients.yaml")
	tokenPath := filepath.Join(dir, "app.token")
	if err := manageClient("issue", registry, "test-app", tokenPath, "llm-gateway.chat,llm-gateway.models.read"); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(tokenPath)
	if st.Mode().Perm() != 0600 {
		t.Fatal("token not private")
	}
	keys, err := platform.LoadClientKeys(registry)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(string(token)))))
	if len(keys) != 1 || keys[0].ID != "test-app" || keys[0].SHA256 != want {
		t.Fatal("credential identity/hash mismatch")
	}
	raw, _ := os.ReadFile(registry)
	if strings.Contains(string(raw), strings.TrimSpace(string(token))) {
		t.Fatal("raw credential leaked")
	}
	if err := manageClient("issue", registry, "test-app", filepath.Join(dir, "other.token"), "llm-gateway.*"); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if err := manageClient("issue", registry, "another", tokenPath, "llm-gateway.chat"); err == nil {
		t.Fatal("token overwritten")
	}
	if err := manageClient("revoke", registry, "test-app", "", ""); err != nil {
		t.Fatal(err)
	}
	keys, err = platform.LoadClientKeys(registry)
	if err != nil || len(keys) != 0 {
		t.Fatal("revocation failed", err)
	}
}

func TestAPIOnlyConfiguration(t *testing.T) {
	if _, err := loadMCPConfig("missing.yaml", "", true); err == nil {
		t.Fatal("missing API configuration accepted")
	}
	cfg, err := loadMCPConfig("missing.yaml", "api.yaml", true)
	if err != nil || cfg != nil {
		t.Fatal("API-only mode loaded MCP config", err)
	}
	if code := runServe([]string{"-api-only"}); code == 0 {
		t.Fatal("API-only served without API config")
	}
}
