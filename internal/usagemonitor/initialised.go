package usagemonitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// How a database began; the value of the service_state row 'initialised'.
const (
	BeganMigrated = "migrated"
	BeganInit     = "init"
)

// markInitialised records how the database began. The first mark stands: a later one changes nothing.
func markInitialised(ctx context.Context, q store.Querier, how string) error {
	_, err := q.ExecContext(ctx, "INSERT OR IGNORE INTO service_state(key,value,set_at) VALUES('initialised',?,?)", how, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Init marks an empty database as the start of a unit with no history to bring over (the explicit
// alternative to migrate-from). It refuses a database that already holds rows in any of the history
// tables and has no mark (marking it would hide how it began). It returns how the database began, which
// is the earlier mark if there was one.
func Init(ctx context.Context, st *store.Store) (string, error) {
	err := st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_state WHERE key='initialised'").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		for _, t := range legacyTables {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.name).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("the database already has rows in %s and no mark: init is for an empty database", t.name)
			}
		}
		return markInitialised(ctx, tx, BeganInit)
	})
	if err != nil {
		return "", err
	}
	return began(ctx, st)
}

func began(ctx context.Context, st *store.Store) (string, error) {
	var how string
	err := st.Reader().QueryRowContext(ctx, "SELECT value FROM service_state WHERE key='initialised'").Scan(&how)
	return how, err
}

// ErrNotInitialised is returned by RequireInitialised.
var ErrNotInitialised = errors.New("the usage database has not been initialised")

// RequireInitialised is the start guard of `serve`: it passes only when `migrate-from` or `init` marked
// the database. A database that holds an owner and keys but neither would start empty and seed the
// configured accounts, and `migrate-from` then refuses ("already has rows"). The data directory has to
// be redone from the identity setup.
func RequireInitialised(ctx context.Context, st *store.Store) error {
	_, err := began(ctx, st)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the initialisation mark: %w", err)
	}
	if err != nil {
		return fmt.Errorf("%w: run `usage-monitor migrate-from --old-db <copy of the old usage.db>` to bring the history over, or `usage-monitor init` to start with none", ErrNotInitialised)
	}
	return nil
}
