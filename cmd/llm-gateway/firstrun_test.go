package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	// A second run changes nothing and succeeds: the owner and every key file are skipped, said so,
	// and the key files are byte for byte what they were.
	snapshot := func() map[string]string {
		m := map[string]string{}
		ents, _ := os.ReadDir(keys)
		for _, e := range ents {
			b, _ := os.ReadFile(filepath.Join(keys, e.Name()))
			m[e.Name()] = string(b)
		}
		return m
	}
	before := snapshot()
	out, err = run()
	if err != nil || !strings.Contains(out, "owner already exists, skipped") || !strings.Contains(out, "already exists, skipped") || strings.Contains(out, "written to") {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("the second run changed the key files")
	}
	// After a partial failure (one key file missing) only that key is made, and the others are kept.
	var lost string
	for name := range before {
		lost = name
		break
	}
	if err = os.Remove(filepath.Join(keys, lost)); err != nil {
		t.Fatal(err)
	}
	out, err = run()
	if err != nil || strings.Count(out, "written to") != 1 || !strings.Contains(out, lost) {
		t.Fatalf("recovery run: %v\n%s", err, out)
	}
	for name, body := range snapshot() {
		if name != lost && body != before[name] {
			t.Fatalf("%s was rewritten", name)
		}
	}
}
