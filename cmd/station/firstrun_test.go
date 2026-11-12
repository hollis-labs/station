package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scripts/first-run sets a unit up offline: an owner, then one 0600 key file per client. Run here
// for the two MCP-host units against throwaway data directories with the real binary.
func TestFirstRunScript(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "station")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	script, err := filepath.Abs("../../scripts/first-run")
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(tmp, "unit.yaml")
	if err := os.WriteFile(cfg, []byte("identity: {bcrypt_cost: 4}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(tmp, "password")
	if err := os.WriteFile(pw, []byte("a long test passphrase\nignored second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(unit, data, keys string, extra ...string) (string, error) {
		c := exec.Command("bash", append([]string{script, unit}, extra...)...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + guardRoot, "FIRST_RUN_BIN=" + bin, "SUBSTRATE_CONFIG=" + cfg,
			"FIRST_RUN_DATA_DIR=" + data, "FIRST_RUN_PASSWORD_FILE=" + pw, "FIRST_RUN_KEYS_DIR=" + keys}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	newKeysDir := func() string {
		d := filepath.Join(t.TempDir(), "keys")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}

	for unit, clients := range map[string][]string{"avatar-station": {"tether-avatar"}, "design-station": {"tether-design"}} {
		t.Run(unit, func(t *testing.T) {
			data, keys := filepath.Join(t.TempDir(), "data"), newKeysDir()
			out, err := run(unit, data, keys)
			if err != nil {
				t.Fatalf("first run: %v\n%s", err, out)
			}
			if strings.Contains(out, "a long test passphrase") {
				t.Fatal("the password appears in the script output")
			}
			for _, c := range clients {
				f := filepath.Join(keys, c+".key")
				st, err := os.Stat(f)
				if err != nil || st.Mode().Perm() != 0o600 {
					t.Fatalf("key file %s: %v %v", f, err, st)
				}
				body, _ := os.ReadFile(f)
				if !strings.HasPrefix(string(body), "key_") {
					t.Fatalf("key file %s does not hold a key", f)
				}
			}
			if _, err := os.Stat(filepath.Join(data, "station.db")); err != nil {
				t.Fatalf("the database is not in the explicit data directory: %v", err)
			}
			// A second run changes nothing and succeeds: the owner and every key file are skipped, said
			// so, and the key files are byte for byte what they were.
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
			out, err = run(unit, data, keys)
			if err != nil || !strings.Contains(out, "owner already exists, skipped") || strings.Contains(out, "written to") {
				t.Fatalf("second run: %v\n%s", err, out)
			}
			if after := snapshot(); !reflect.DeepEqual(before, after) {
				t.Fatal("the second run changed the key files")
			}
			// After a partial failure (a key file missing) only that key is made.
			var lost string
			for name := range before {
				lost = name
				break
			}
			if err = os.Remove(filepath.Join(keys, lost)); err != nil {
				t.Fatal(err)
			}
			out, err = run(unit, data, keys)
			if err != nil || strings.Count(out, "written to") != 1 || !strings.Contains(out, lost) {
				t.Fatalf("recovery run: %v\n%s", err, out)
			}
			for name, body := range snapshot() {
				if name != lost && body != before[name] {
					t.Fatalf("%s was rewritten", name)
				}
			}
		})
	}

	// Refusals: no explicit data directory, a relative one, a loose-mode password file, an unknown unit.
	keys := newKeysDir()
	if out, err := run("avatar-station", "", keys); err == nil || !strings.Contains(out, "FIRST_RUN_DATA_DIR") {
		t.Fatalf("an empty data directory was accepted:\n%s", out)
	}
	if out, err := run("avatar-station", "relative/dir", keys); err == nil || !strings.Contains(out, "absolute") {
		t.Fatalf("a relative data directory was accepted:\n%s", out)
	}
	if out, err := run("nonsense", filepath.Join(t.TempDir(), "d"), keys); err == nil || !strings.Contains(out, "unknown unit") {
		t.Fatalf("an unknown unit was accepted:\n%s", out)
	}
	if err := os.Chmod(pw, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("avatar-station", filepath.Join(t.TempDir(), "d"), keys); err == nil || !strings.Contains(out, "0600") {
		t.Fatalf("a loose password file was accepted:\n%s", out)
	}
}
