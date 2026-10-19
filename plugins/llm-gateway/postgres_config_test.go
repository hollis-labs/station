package llmgateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func structuredPGFixture() PostgresConfig {
	return PostgresConfig{Host: "postgres.example.test", Port: 5432, Database: "ledger", User: "ledger_runtime", PasswordRef: "file:///private/runtime_password"}
}

func TestStructuredPostgresCredentialAndTLS(t *testing.T) {
	c := structuredPGFixture()
	canary := "private @:/?&password"
	d, e := c.resolveDSN(context.Background(), func(_ context.Context, ref string) (string, error) {
		if ref != c.PasswordRef {
			t.Fatal("wrong reference")
		}
		return canary, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	p, e := pgx.ParseConfig(d)
	if e != nil {
		t.Fatal("generated config did not parse")
	}
	if p.Password != canary || p.User != c.User || p.Host != c.Host || p.Database != c.Database || p.TLSConfig == nil || p.TLSConfig.InsecureSkipVerify || p.TLSConfig.ServerName != c.Host || len(p.Fallbacks) != 0 || p.RuntimeParams["search_path"] != "pg_catalog,public" {
		t.Fatal("connection identity, TLS or trusted schema not preserved")
	}
	if c.schemaMode() != "verify" || (PostgresConfig{DSNRef: "file:///private/dsn"}).schemaMode() != "auto" {
		t.Fatal("unsafe default or legacy behavior changed")
	}
}

func TestPostgresConfigurationRefusesAmbiguityAndSecretErrors(t *testing.T) {
	for _, mutate := range []func(*PostgresConfig){func(c *PostgresConfig) { c.DSNRef = "file:///dsn" }, func(c *PostgresConfig) { c.PasswordRef = "literal" }, func(c *PostgresConfig) { c.Port = 0 }, func(c *PostgresConfig) { c.Schema = "public,untrusted" }, func(c *PostgresConfig) { c.SchemaMode = "skip" }, func(c *PostgresConfig) { c.Host = "/tmp/socket" }} {
		c := structuredPGFixture()
		mutate(&c)
		if c.validate() == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	for _, c := range []PostgresConfig{structuredPGFixture(), {DSNRef: "file:///private/dsn"}} {
		_, e := c.resolveDSN(context.Background(), func(context.Context, string) (string, error) { return "", errors.New("credential-canary") })
		if e == nil || strings.Contains(e.Error(), "credential-canary") {
			t.Fatal("resolver error leaked")
		}
	}
}
