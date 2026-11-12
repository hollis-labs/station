package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/first-run sets the gateway unit up offline: an owner, then one 0600 key file per client,
// in an explicit throwaway data directory with the real binary.
func TestFirstRunScript(t *testing.T) {
	script, err := filepath.Abs("../../scripts/first-run")
	if err != nil {
		t.Fatal(err)
	}
	u := newUnit(t, "")
	pw := writeFile(t, "password", "a long test passphrase\nignored second line\n")
	keys := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func() (string, error) {
		c := exec.Command("bash", script, "llm-gateway")
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + guardRoot, "FIRST_RUN_BIN=" + gatewayBin, "SUBSTRATE_CONFIG=" + u.cfg,
			"FIRST_RUN_DATA_DIR=" + u.dir, "FIRST_RUN_PASSWORD_FILE=" + pw, "FIRST_RUN_KEYS_DIR=" + keys}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	if strings.Contains(out, "a long test passphrase") {
		t.Fatal("the password appears in the script output")
	}
	for _, c := range []string{"station-operator", "station-smoke", "usage-collector"} {
		f := filepath.Join(keys, c+".key")
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file %s: %v %v", f, err, st)
		}
		if body, _ := os.ReadFile(f); !strings.HasPrefix(string(body), "key_") {
			t.Fatalf("key file %s does not hold a key", f)
		}
	}
	if _, err := os.Stat(filepath.Join(u.dir, "llm-gateway.db")); err != nil {
		t.Fatalf("the database is not in the explicit data directory: %v", err)
	}
	// A second run changes nothing: the owner exists, so setup refuses and no key file appears.
	before, _ := os.ReadDir(keys)
	if out, err := run(); err == nil {
		t.Fatalf("second run succeeded:\n%s", out)
	}
	if after, _ := os.ReadDir(keys); len(before) != len(after) {
		t.Fatal("the second run changed the key files")
	}
}
