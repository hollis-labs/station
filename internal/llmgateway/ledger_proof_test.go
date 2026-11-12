package llmgateway

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// liveLedger reproduces the live Postgres role layout of the ledger in a throwaway database of
// the fixture server (STATION_TEST_PG_DSN, a superuser):
//
//	owner     NOLOGIN, owns the objects
//	migrator  LOGIN, NOINHERIT, member of owner WITH SET (it runs SET ROLE owner to migrate)
//	rgroup    NOLOGIN, holds the runtime grants
//	runtime   LOGIN, member of rgroup, no CREATE on the schema
type liveLedger struct {
	admin                               *pgx.Conn // superuser, connected to the throwaway database
	owner, migrator, rgroup, runtimeUse string
	runtimeDSN, migrationDSN            string
}

func newLiveLedger(t *testing.T) *liveLedger {
	t.Helper()
	ctx := context.Background()
	base := pgDSN(t)
	sys, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_")
	id = fmt.Sprintf("%s_%d", id[:min(len(id), 20)], time.Now().UnixNano()%1_000_000)
	db := "proof_" + id
	l := &liveLedger{owner: "own_" + id, migrator: "mig_" + id, rgroup: "rg_" + id, runtimeUse: "rt_" + id}
	if _, err := sys.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	roles := []string{l.owner, l.migrator, l.rgroup, l.runtimeUse}
	t.Cleanup(func() {
		if l.admin != nil {
			_ = l.admin.Close(ctx)
		}
		_, _ = sys.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		for _, r := range roles {
			_, _ = sys.Exec(ctx, "DROP ROLE IF EXISTS "+r)
		}
		_ = sys.Close(ctx)
	})
	dsnFor := func(user string) string {
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "postgres" {
			t.Fatal("fixture needs a postgres URL")
		}
		u.Path = "/" + db
		if user != "" {
			u.User = url.User(user)
		}
		return u.String()
	}
	if l.admin, err = pgx.Connect(ctx, dsnFor("")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE ROLE " + l.owner + " NOLOGIN",
		"CREATE ROLE " + l.migrator + " LOGIN NOINHERIT",
		"GRANT " + l.owner + " TO " + l.migrator + " WITH INHERIT FALSE, SET TRUE",
		"CREATE ROLE " + l.rgroup + " NOLOGIN",
		"CREATE ROLE " + l.runtimeUse + " LOGIN",
		"GRANT " + l.rgroup + " TO " + l.runtimeUse,
		"GRANT CONNECT ON DATABASE " + db + " TO " + l.migrator + ", " + l.runtimeUse,
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT CREATE, USAGE ON SCHEMA public TO " + l.owner,
		"GRANT USAGE ON SCHEMA public TO " + l.rgroup,
	} {
		if _, err := l.admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	l.runtimeDSN, l.migrationDSN = dsnFor(l.runtimeUse), dsnFor(l.migrator)
	return l
}

// migrate runs the ledger-migrate code path as the migration login.
func (l *liveLedger) migrate(ctx context.Context) error {
	return MigratePostgresLedger(ctx, PostgresConfig{DSNRef: "file:///proof/migration"}, l.owner,
		func(context.Context, string) (string, error) { return l.migrationDSN, nil })
}

func (l *liveLedger) grantRuntime(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"GRANT INSERT, SELECT, UPDATE ON llm_calls TO " + l.rgroup,
		"GRANT SELECT ON llm_ledger_schema TO " + l.rgroup,
	} {
		if _, err := l.admin.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func (l *liveLedger) versions(t *testing.T) []int {
	t.Helper()
	rows, err := l.admin.Query(context.Background(), "SELECT version FROM llm_ledger_schema ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (l *liveLedger) can(t *testing.T, query string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := l.admin.QueryRow(context.Background(), query, args...).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// Proof for the port: the Postgres ledger code is unchanged (three migrations, v1 to v3, no v4),
// and the ported gateway works against the live role layout. Skipped without STATION_TEST_PG_DSN.
func TestPGLiveRoleLayout(t *testing.T) {
	ctx := context.Background()
	l := newLiveLedger(t)

	if len(pgMigrations) != 3 {
		t.Fatalf("the port carries %d ledger migrations, the live schema is v1 to v3", len(pgMigrations))
	}
	if err := l.migrate(ctx); err != nil {
		t.Fatalf("ledger-migrate as the migration login: %v", err)
	}
	if got := fmt.Sprint(l.versions(t)); got != "[1 2 3]" {
		t.Fatalf("schema versions after migrating from empty: %s", got)
	}
	var owner string
	if err := l.admin.QueryRow(ctx, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='llm_calls'::regclass").Scan(&owner); err != nil || owner != l.owner {
		t.Fatalf("llm_calls owner = %q (%v), want the NOLOGIN owner role: SET ROLE did not take effect", owner, err)
	}
	l.grantRuntime(t)

	// The runtime login can insert, select and update llm_calls and select the schema table; it cannot
	// delete, alter, create, or write the schema table.
	rt := l.runtimeUse
	for _, c := range []struct {
		what string
		got  bool
		want bool
	}{
		{"insert llm_calls", l.can(t, "SELECT has_table_privilege($1,'llm_calls','INSERT')", rt), true},
		{"select llm_calls", l.can(t, "SELECT has_table_privilege($1,'llm_calls','SELECT')", rt), true},
		{"update llm_calls", l.can(t, "SELECT has_table_privilege($1,'llm_calls','UPDATE')", rt), true},
		{"delete llm_calls", l.can(t, "SELECT has_table_privilege($1,'llm_calls','DELETE')", rt), false},
		{"select llm_ledger_schema", l.can(t, "SELECT has_table_privilege($1,'llm_ledger_schema','SELECT')", rt), true},
		{"insert llm_ledger_schema", l.can(t, "SELECT has_table_privilege($1,'llm_ledger_schema','INSERT')", rt), false},
		{"create in schema", l.can(t, "SELECT has_schema_privilege($1,'public','CREATE')", rt), false},
	} {
		if c.got != c.want {
			t.Errorf("runtime login: %s = %v, want %v", c.what, c.got, c.want)
		}
	}

	// The ported gateway starts in verify mode as the runtime login, serves a request and writes
	// its ledger row.
	t.Setenv("PROOF_LEDGER_DSN", l.runtimeDSN)
	extra := `    ledger:
      store: postgres
      postgres: {dsn_env: PROOF_LEDGER_DSN, schema_mode: verify}`
	e := newGW(t, openaiOK, openaiOK, anthropicOK, cfgOpts{extra: extra})
	if resp, body := e.do(t, "POST", "/v1/chat/completions", oaAuth, oaReq); resp.StatusCode != 200 {
		t.Fatalf("chat = %d %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(8 * time.Second)
	var rows int
	for time.Now().Before(deadline) && rows == 0 {
		if err := l.admin.QueryRow(ctx, "SELECT count(*) FROM llm_calls WHERE principal='caller-1'").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rows == 0 {
		t.Fatal("the gateway wrote no ledger row as the runtime login")
	}
	t.Logf("gateway (verify mode, runtime login) wrote %d llm_calls row(s)", rows)

	// verify refuses when the recorded schema is not exactly v1 to v3.
	mustRefuse := func(label string) {
		t.Helper()
		s, err := openPGStoreMode(ctx, l.runtimeDSN, "verify")
		if err == nil {
			s.Close()
			t.Fatalf("verify accepted: %s", label)
		}
		t.Logf("verify refuses %s: %v", label, err)
	}
	for _, c := range []struct{ label, break_, restore string }{
		{"a missing v3", "DELETE FROM llm_ledger_schema WHERE version=3", "INSERT INTO llm_ledger_schema(version) VALUES (3)"},
		{"a newer v4", "INSERT INTO llm_ledger_schema(version) VALUES (4)", "DELETE FROM llm_ledger_schema WHERE version=4"},
	} {
		if _, err := l.admin.Exec(ctx, c.break_); err != nil {
			t.Fatal(err)
		}
		mustRefuse(c.label)
		if _, err := l.admin.Exec(ctx, c.restore); err != nil {
			t.Fatal(err)
		}
	}
	if s, err := openPGStoreMode(ctx, l.runtimeDSN, "verify"); err != nil {
		t.Fatalf("verify after restoring: %v", err)
	} else {
		s.Close()
	}

	// auto under the runtime login (no CREATE): what it does, recorded, and it changes nothing.
	if s, err := openPGStoreMode(ctx, l.runtimeDSN, "auto"); err == nil {
		s.Close()
		t.Fatal("auto succeeded under a login without CREATE")
	} else {
		t.Logf("auto under the runtime login: %v", err)
	}
	if got := fmt.Sprint(l.versions(t)); got != "[1 2 3]" {
		t.Fatalf("schema versions changed by a refused auto start: %s", got)
	}
	// A rerun of the migration is a no-op.
	if err := l.migrate(ctx); err != nil {
		t.Fatalf("ledger-migrate rerun: %v", err)
	}
	if got := fmt.Sprint(l.versions(t)); got != "[1 2 3]" {
		t.Fatalf("schema versions after the rerun: %s", got)
	}
	// The runtime login cannot change the schema.
	rtConn, err := pgx.Connect(ctx, l.runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer rtConn.Close(ctx) //nolint:errcheck
	if _, err := rtConn.Exec(ctx, "ALTER TABLE llm_calls ADD COLUMN x integer"); err == nil {
		t.Fatal("the runtime login altered the ledger table")
	}
	// And the migration login without SET ROLE cannot create objects (it owns nothing).
	mig, err := pgx.Connect(ctx, l.migrationDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer mig.Close(ctx) //nolint:errcheck
	if _, err := mig.Exec(ctx, "CREATE TABLE direct_by_migrator (x int)"); err == nil {
		t.Fatal("the migration login created an object without SET ROLE")
	}
}
