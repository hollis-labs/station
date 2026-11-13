package appcmd

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: filepath.Join(t.TempDir(), "x.db"), CreateParentDir: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func exec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

const credTable = `CREATE TABLE identity_credentials (id TEXT PRIMARY KEY, name TEXT NOT NULL, revoked_at BIGINT NOT NULL DEFAULT 0, expires_at BIGINT NOT NULL DEFAULT 0)`

func TestIssuedKeyPrincipals(t *testing.T) {
	ctx := context.Background()

	// A unit that never ran identity has no keys, and that is not an error.
	if got, err := IssuedKeyPrincipals(ctx, openTemp(t)); err != nil || len(got) != 0 {
		t.Fatalf("no identity tables: %v %v", got, err)
	}

	// Only live keys count: not revoked, not expired.
	st := openTemp(t)
	exec(t, st, `CREATE TABLE identity_users (id TEXT PRIMARY KEY)`)
	exec(t, st, credTable)
	now := time.Now().Unix()
	exec(t, st, `INSERT INTO identity_credentials(id,name) VALUES('live','a')`)
	exec(t, st, `INSERT INTO identity_credentials(id,name,expires_at) VALUES('future','b',?)`, now+3600)
	exec(t, st, `INSERT INTO identity_credentials(id,name,revoked_at) VALUES('revoked','c',?)`, now-5)
	exec(t, st, `INSERT INTO identity_credentials(id,name,expires_at) VALUES('expired','d',?)`, now-5)
	got, err := IssuedKeyPrincipals(ctx, st)
	slices.Sort(got)
	if err != nil || !slices.Equal(got, []string{"key:future", "key:live"}) {
		t.Fatalf("live keys = %v (%v)", got, err)
	}

	// A schema that is not the one this reads is an error, never "no keys".
	broken := openTemp(t)
	exec(t, broken, `CREATE TABLE identity_users (id TEXT PRIMARY KEY)`)
	if got, err := IssuedKeyPrincipals(ctx, broken); err == nil {
		t.Fatalf("identity_users without identity_credentials = %v, want an error", got)
	}
	changed := openTemp(t)
	exec(t, changed, `CREATE TABLE identity_credentials (id TEXT PRIMARY KEY, name TEXT NOT NULL)`)
	if got, err := IssuedKeyPrincipals(ctx, changed); err == nil {
		t.Fatalf("credentials without the revoked/expires columns = %v, want an error", got)
	}
}
