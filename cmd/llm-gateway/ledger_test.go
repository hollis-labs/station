package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The ledger-migrate command with the real binary against a throwaway Postgres (STATION_TEST_PG_DSN,
// a superuser; skipped without it): the migration DSN comes from the unit's secrets store, the
// objects end up owned by the NOLOGIN owner role through SET ROLE, and a rerun changes nothing.
func TestLedgerMigrateCommand(t *testing.T) {
	base := os.Getenv("STATION_TEST_PG_DSN")
	if base == "" {
		t.Skip("STATION_TEST_PG_DSN not set; skipping Postgres tests")
	}
	ctx := context.Background()
	sys, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer sys.Close(ctx) //nolint:errcheck
	id := fmt.Sprint(time.Now().UnixNano() % 1_000_000)
	db, owner, migrator := "cmdproof_"+id, "cmd_own_"+id, "cmd_mig_"+id
	t.Cleanup(func() {
		_, _ = sys.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		_, _ = sys.Exec(ctx, "DROP ROLE IF EXISTS "+migrator)
		_, _ = sys.Exec(ctx, "DROP ROLE IF EXISTS "+owner)
	})
	for _, q := range []string{
		"CREATE DATABASE " + db,
		"CREATE ROLE " + owner + " NOLOGIN",
		"CREATE ROLE " + migrator + " LOGIN NOINHERIT",
		"GRANT " + owner + " TO " + migrator + " WITH INHERIT FALSE, SET TRUE",
		"GRANT CONNECT ON DATABASE " + db + " TO " + migrator,
	} {
		if _, err := sys.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	u, _ := url.Parse(base)
	u.Path = "/" + db
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx) //nolint:errcheck
	for _, q := range []string{"REVOKE CREATE ON SCHEMA public FROM PUBLIC", "GRANT CREATE, USAGE ON SCHEMA public TO " + owner} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	u.User = url.User(migrator)

	unit := newUnit(t, "")
	unit.mustRun(u.String()+"\n", "secret", "set", "file://ledger/migration")
	for i := 0; i < 2; i++ { // the second run is a no-op
		out := unit.mustRun("", "ledger-migrate", "--dsn-ref", "file://ledger/migration", "--owner-role", owner)
		if !strings.Contains(out, "ledger schema is current") {
			t.Fatalf("run %d: %s", i+1, out)
		}
	}
	var versions, tableOwner string
	if err := admin.QueryRow(ctx, "SELECT string_agg(version::text, ',' ORDER BY version) FROM llm_ledger_schema").Scan(&versions); err != nil || versions != "1,2,3" {
		t.Fatalf("versions = %q (%v)", versions, err)
	}
	if err := admin.QueryRow(ctx, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='llm_calls'::regclass").Scan(&tableOwner); err != nil || tableOwner != owner {
		t.Fatalf("llm_calls owner = %q (%v), want %s", tableOwner, err, owner)
	}
	// Without --owner-role the command refuses before connecting.
	if out, err := unit.run("", "ledger-migrate", "--dsn-ref", "file://ledger/migration"); err == nil || !strings.Contains(out, "--owner-role") {
		t.Fatalf("ledger-migrate without --owner-role: %v\n%s", err, out)
	}
}
