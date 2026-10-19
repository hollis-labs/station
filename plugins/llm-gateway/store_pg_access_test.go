package llmgateway

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Run only against the disposable PostgreSQL fixture used by ledger tests.
func TestPGRuntimeVerificationAndSeparateMigration(t *testing.T) {
	ctx := context.Background()
	dsn := pgDSN(t)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("fixture admin unavailable")
	}
	defer admin.Close(ctx) //nolint:errcheck
	exec := func(q string) {
		t.Helper()
		if _, e := admin.Exec(ctx, q); e != nil {
			t.Fatal(e)
		}
	}
	exec(`CREATE ROLE fixture_ledger_owner NOLOGIN; CREATE ROLE fixture_ledger_migrator LOGIN NOINHERIT; CREATE ROLE fixture_ledger_runtime LOGIN; GRANT fixture_ledger_owner TO fixture_ledger_migrator WITH INHERIT FALSE, SET TRUE; CREATE SCHEMA runtime_fixture AUTHORIZATION fixture_ledger_owner`)
	defer func() {
		_, e := admin.Exec(ctx, `DROP SCHEMA runtime_fixture CASCADE; DROP ROLE fixture_ledger_runtime; DROP ROLE fixture_ledger_migrator; DROP ROLE fixture_ledger_owner`)
		if e != nil {
			t.Error(e)
		}
	}()
	connection := func(user string) string {
		u, e := url.Parse(dsn)
		if e != nil || u.Scheme != "postgres" {
			t.Fatal("fixture needs postgres URL")
		}
		u.User = url.User(user)
		q := u.Query()
		q.Set("search_path", "pg_catalog,runtime_fixture")
		u.RawQuery = q.Encode()
		return u.String()
	}
	runtimeDSN, migrationDSN := connection("fixture_ledger_runtime"), connection("fixture_ledger_migrator")
	exec(`GRANT USAGE ON SCHEMA runtime_fixture TO fixture_ledger_runtime`)
	if s, e := openPGStoreMode(ctx, runtimeDSN, "verify"); e == nil {
		s.Close()
		t.Fatal("empty schema accepted")
	}
	migrate := func() error {
		return MigratePostgresLedger(ctx, PostgresConfig{DSNRef: "file:///fixture/migration"}, "fixture_ledger_owner", func(context.Context, string) (string, error) { return migrationDSN, nil })
	}
	if e := migrate(); e != nil {
		t.Fatal(e)
	}
	exec(`GRANT SELECT,INSERT,UPDATE,DELETE ON runtime_fixture.llm_calls TO fixture_ledger_runtime; GRANT SELECT ON runtime_fixture.llm_ledger_schema TO fixture_ledger_runtime`)
	var owner string
	if e := admin.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='runtime_fixture.llm_calls'::regclass`).Scan(&owner); e != nil || owner != "fixture_ledger_owner" {
		t.Fatal("migration did not create owner-owned objects")
	}
	s, e := openPGStoreMode(ctx, runtimeDSN, "verify")
	if e != nil {
		t.Fatal(e)
	}
	runStoreContract(t, s)
	s.Close()
	// An already migrated schema must work on a fresh runtime connection too.
	s, e = openPGStoreMode(ctx, runtimeDSN, "verify")
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = openPGStoreMode(ctx, runtimeDSN, "auto"); e == nil {
		s.Close()
		t.Fatal("runtime unexpectedly has migration privilege")
	}
	if e = migrate(); e != nil {
		t.Fatal("migration rerun failed", e)
	}
	for _, change := range []struct{ apply, undo string }{
		{`DELETE FROM runtime_fixture.llm_ledger_schema WHERE version=2`, `INSERT INTO runtime_fixture.llm_ledger_schema(version) VALUES(2)`},
		{`INSERT INTO runtime_fixture.llm_ledger_schema(version) VALUES(999)`, `DELETE FROM runtime_fixture.llm_ledger_schema WHERE version=999`},
		{`ALTER TABLE runtime_fixture.llm_calls RENAME COLUMN cost_bound TO hidden_bound`, `ALTER TABLE runtime_fixture.llm_calls RENAME COLUMN hidden_bound TO cost_bound`},
	} {
		exec(change.apply)
		s, e = openPGStoreMode(ctx, runtimeDSN, "verify")
		if e == nil {
			s.Close()
			t.Error("incompatible schema accepted")
		}
		exec(change.undo)
	}
	// The runtime login cannot obtain migration authority through this command.
	e = MigratePostgresLedger(ctx, PostgresConfig{DSNRef: "file:///fixture/runtime"}, "fixture_ledger_owner", func(context.Context, string) (string, error) { return runtimeDSN, nil })
	if e == nil {
		t.Fatal("runtime assumed owner")
	}
}
