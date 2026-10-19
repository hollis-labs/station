package llmgateway

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hollis-labs/libs/security/secrets"
)

var pgIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (c PostgresConfig) structured() bool {
	return c.Host != "" || c.Port != 0 || c.Database != "" || c.User != "" || c.PasswordRef != "" || c.Schema != ""
}
func (c PostgresConfig) schemaMode() string {
	if c.SchemaMode != "" {
		return c.SchemaMode
	}
	if c.structured() {
		return "verify"
	}
	return "auto"
}
func (c PostgresConfig) validate() error {
	if c.SchemaMode != "" && c.SchemaMode != "auto" && c.SchemaMode != "verify" {
		return fmt.Errorf("llm-gateway: ledger.postgres.schema_mode must be auto or verify")
	}
	if !c.structured() {
		if (c.DSNRef == "") == (c.DSNEnv == "") {
			return fmt.Errorf("llm-gateway: ledger.postgres needs one DSN source or a complete structured connection")
		}
		return nil
	}
	if c.DSNRef != "" || c.DSNEnv != "" {
		return fmt.Errorf("llm-gateway: ledger.postgres cannot mix a DSN with structured connection fields")
	}
	if c.Host == "" || strings.ContainsAny(c.Host, " /\\\t\r\n@?#,") || c.Port < 1 || c.Port > 65535 || !pgIdentifier.MatchString(c.Database) || !pgIdentifier.MatchString(c.User) || (c.Schema != "" && !pgIdentifier.MatchString(c.Schema)) {
		return fmt.Errorf("llm-gateway: ledger.postgres needs a hostname, port and bounded database/user/schema identifiers")
	}
	if _, err := secrets.ParseRef(c.PasswordRef); err != nil {
		return fmt.Errorf("llm-gateway: ledger.postgres needs a valid password_ref")
	}
	return nil
}

type resolvePostgresSecret func(context.Context, string) (string, error)

func (c PostgresConfig) resolveDSN(ctx context.Context, resolve resolvePostgresSecret) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	if !c.structured() {
		if c.DSNRef != "" {
			d, e := resolve(ctx, c.DSNRef)
			if e != nil {
				return "", fmt.Errorf("llm-gateway: ledger DSN reference could not be resolved")
			}
			if d == "" {
				return "", fmt.Errorf("llm-gateway: ledger DSN reference is empty")
			}
			return d, nil
		}
		d := os.Getenv(c.DSNEnv)
		if d == "" {
			return "", fmt.Errorf("llm-gateway: ledger DSN environment variable is empty")
		}
		return d, nil
	}
	password, err := resolve(ctx, c.PasswordRef)
	if err != nil || password == "" {
		return "", fmt.Errorf("llm-gateway: ledger password reference could not be resolved")
	}
	schema := c.Schema
	if schema == "" {
		schema = "public"
	}
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), Path: "/" + c.Database, User: url.UserPassword(c.User, password)}
	q := url.Values{"sslmode": {"verify-full"}, "search_path": {"pg_catalog," + schema}, "application_name": {"station_llm_gateway"}, "connect_timeout": {"5"}}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// MigratePostgresLedger applies ledger schema changes without starting the API,
// loading provider credentials or constructing any provider clients. The caller
// supplies a migration identity and the NOLOGIN owner it may SET ROLE to.
func MigratePostgresLedger(ctx context.Context, c PostgresConfig, owner string, resolve func(context.Context, string) (string, error)) error {
	if !pgIdentifier.MatchString(owner) {
		return fmt.Errorf("llm-gateway: ledger migration requires a bounded owner role")
	}
	dsn, err := c.resolveDSN(ctx, resolve)
	if err != nil {
		return err
	}
	s, err := connectPGStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.migrateAs(ctx, owner)
}
