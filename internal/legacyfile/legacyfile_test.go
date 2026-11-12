package legacyfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
)

// tree is a listing of dir: every path with its size, mode and content digest.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		sum := ""
		if !d.IsDir() {
			raw, _ := os.ReadFile(p)
			h := sha256.Sum256(raw)
			sum = hex.EncodeToString(h[:])
		}
		fmt.Fprintf(&b, "%s %d %v %s\n", p, info.Size(), info.Mode(), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// liveOld builds what a crashed or running old unit leaves behind: an old-format database whose
// application_id still sits in the -wal sidecar (never checkpointed), copied into dir. It returns
// the path of the main file.
func liveOld(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "old.db")
	db, err := store.Open(t.Context(), store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: src}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE t(x); INSERT INTO t VALUES(1); PRAGMA application_id=1398035789; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "old.db")
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatalf("the fixture needs a %s file: %v", suffix, err)
		}
		if err = os.WriteFile(dst+suffix, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestApplicationIDNeverAltersTheFile(t *testing.T) {
	dir := t.TempDir()
	path := liveOld(t, dir)
	hdr, _ := os.ReadFile(path)
	if id := int64(hdr[68])<<24 | int64(hdr[69])<<16 | int64(hdr[70])<<8 | int64(hdr[71]); id != 0 {
		t.Fatalf("the fixture is not discriminating: the header already holds application_id %d, so the -wal path is not exercised", id)
	}
	before := tree(t, dir)
	id, known, err := ApplicationID(t.Context(), path)
	if err != nil || !known || id != 1398035789 {
		t.Fatalf("ApplicationID = %d, %v, %v; want 1398035789 from the -wal", id, known, err)
	}
	if after := tree(t, dir); after != before {
		t.Fatalf("the file or its directory changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestApplicationIDReadsAHeaderWithoutOpeningTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.db")
	db, err := store.Open(t.Context(), store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE t(x); PRAGMA application_id=42"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // a clean close checkpoints and removes the sidecars
	before := tree(t, dir)
	if id, known, err := ApplicationID(t.Context(), path); err != nil || !known || id != 42 {
		t.Fatalf("ApplicationID = %d, %v, %v", id, known, err)
	}
	if tree(t, dir) != before {
		t.Fatal("reading the application id changed the directory")
	}
	for name, want := range map[string]bool{"missing.db": false} {
		if _, known, err := ApplicationID(t.Context(), filepath.Join(dir, name)); err != nil || known != want {
			t.Fatalf("%s: known=%v err=%v", name, known, err)
		}
	}
	notDB := filepath.Join(dir, "text.txt")
	_ = os.WriteFile(notDB, []byte(strings.Repeat("x", 300)), 0o600)
	if _, known, _ := ApplicationID(t.Context(), notDB); known {
		t.Fatal("a text file was taken for a database")
	}
}

func TestTakeCopiesTheWALAndLeavesTheSourceAlone(t *testing.T) {
	dir := t.TempDir()
	path := liveOld(t, dir)
	before := tree(t, dir)
	snap, err := Take(path)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	db, err := store.Open(t.Context(), store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: snap.Path, ReadConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.Reader().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the copy lost what only the -wal held: %d %v", n, err)
	}
	_ = db.Close()
	if same, err := snap.Unchanged(); err != nil || !same {
		t.Fatalf("Unchanged = %v, %v", same, err)
	}
	if tree(t, dir) != before {
		t.Fatal("taking the snapshot changed the source directory")
	}
	// A source that changes is reported.
	if err = os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if same, _ := snap.Unchanged(); same {
		t.Fatal("a changed source was reported unchanged")
	}
}
