package llmgateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/hollis-labs/libs/core/store"
)

// The old gateway database (station's gateway.db) is identified by this SQLite application id and
// user_version. Its four tables are the ones the service database has, column for column.
const (
	legacyApplicationID = 0x53544757
	legacyUserVersion   = 1
)

// LegacyReport is what MigrateLegacy copied.
type LegacyReport struct {
	Revisions   int64  `json:"revisions"`
	Activations int64  `json:"activations"`
	Usage       int64  `json:"usage"`
	Latest      *int64 `json:"latest_revision"`
	Active      *int64 `json:"active_revision"`
}

// MigrateLegacy copies an old gateway.db into the (empty) service database in one transaction:
// revisions verbatim (ids, document text and checksums, so the history stays verifiable), the
// state row, the activation audit and the SQLite usage ledger. Run it on an online-backup copy of
// the old file, never on the live one: opening a file moves it to WAL and may create sidecar files.
// Counts and every revision checksum are verified inside the transaction, so on any error nothing
// is written to dst.
func MigrateLegacy(ctx context.Context, dst *store.Store, legacyPath string) (LegacyReport, error) {
	var rep LegacyReport
	if _, err := os.Stat(legacyPath); err != nil {
		return rep, err
	}
	cfg := store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: legacyPath, ReadConns: 1}}
	old, err := store.Open(ctx, cfg)
	if err != nil {
		return rep, fmt.Errorf("open the old gateway database: %w", err)
	}
	defer func() { _ = old.Close() }()

	var appID, userVersion int64
	if err := old.Reader().QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return rep, err
	}
	if err := old.Reader().QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return rep, err
	}
	if appID != legacyApplicationID || userVersion != legacyUserVersion {
		return rep, fmt.Errorf("%s is not an old gateway database (application_id %#x, user_version %d)", legacyPath, appID, userVersion)
	}
	for _, table := range []string{"gateway_revisions", "gateway_activations", "gateway_usage"} {
		var n int64
		if err := dst.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return rep, err
		}
		if n != 0 {
			return rep, fmt.Errorf("the service database already has rows in %s: migrate into a new, empty database", table)
		}
	}

	count := func(table string) (n int64, err error) {
		err = old.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
		return n, err
	}
	var wantRev, wantAct, wantUse int64
	for table, into := range map[string]*int64{"gateway_revisions": &wantRev, "gateway_activations": &wantAct, "gateway_usage": &wantUse} {
		if *into, err = count(table); err != nil {
			return rep, err
		}
	}

	// The callback only reads the old file and inserts into dst, so a restart after contention
	// repeats it from the start with nothing half-done.
	err = dst.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		rep = LegacyReport{}
		copyRows := func(query string, insert string, cols int, counter *int64) error {
			rows, err := old.Reader().QueryContext(ctx, query)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			vals := make([]any, cols)
			ptrs := make([]any, cols)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			for rows.Next() {
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, insert, vals...); err != nil {
					return err
				}
				*counter++
			}
			return rows.Err()
		}
		if err := copyRows("SELECT id,created_at,actor,document_version,document,sha256 FROM gateway_revisions ORDER BY id",
			"INSERT INTO gateway_revisions(id,created_at,actor,document_version,document,sha256) VALUES(?,?,?,?,?,?)", 6, &rep.Revisions); err != nil {
			return err
		}
		var latest, active sql.NullInt64
		if err := old.Reader().QueryRowContext(ctx, "SELECT latest_revision,active_revision FROM gateway_state WHERE singleton=1").Scan(&latest, &active); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=?, active_revision=? WHERE singleton=1", nullable(latest), nullable(active)); err != nil {
			return err
		}
		if latest.Valid {
			rep.Latest = &latest.Int64
		}
		if active.Valid {
			rep.Active = &active.Int64
		}
		if err := copyRows("SELECT id,activated_at,actor,previous_revision,revision FROM gateway_activations ORDER BY id",
			"INSERT INTO gateway_activations(id,activated_at,actor,previous_revision,revision) VALUES(?,?,?,?,?)", 5, &rep.Activations); err != nil {
			return err
		}
		if err := copyRows("SELECT id,recorded_at,principal,provider,model,outcome,input_tokens,output_tokens,cost_usd,entry FROM gateway_usage ORDER BY recorded_at,id",
			"INSERT INTO gateway_usage(id,recorded_at,principal,provider,model,outcome,input_tokens,output_tokens,cost_usd,entry) VALUES(?,?,?,?,?,?,?,?,?,?)", 10, &rep.Usage); err != nil {
			return err
		}
		// Verified before the commit, so a mismatch leaves the service database untouched.
		if rep.Revisions != wantRev || rep.Activations != wantAct || rep.Usage != wantUse {
			return fmt.Errorf("copied %d/%d/%d revisions/activations/usage rows, the old file has %d/%d/%d", rep.Revisions, rep.Activations, rep.Usage, wantRev, wantAct, wantUse)
		}
		return verifyChecksums(ctx, tx)
	})
	if err != nil {
		return LegacyReport{}, fmt.Errorf("copy the old gateway database: %w", err)
	}
	return rep, nil
}

// verifyChecksums recomputes every stored revision's checksum from its document text.
func verifyChecksums(ctx context.Context, q store.Querier) error {
	rows, err := q.QueryContext(ctx, "SELECT id,document,sha256 FROM gateway_revisions")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var doc, sum string
		if err := rows.Scan(&id, &doc, &sum); err != nil {
			return err
		}
		if hash := sha256.Sum256([]byte(doc)); hex.EncodeToString(hash[:]) != sum {
			return fmt.Errorf("revision %d: stored checksum does not match its document", id)
		}
	}
	return rows.Err()
}

func nullable(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// RefuseLegacyDatabase is a start guard: it returns an error when path is an old gateway.db, so a
// service never opens (and migrates around) the old file. A missing file is fine. The identity is
// read through SQLite, not from the file header: an old database in WAL mode can keep its header
// change in the -wal file until a checkpoint.
func RefuseLegacyDatabase(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: path, ReadConns: 1}})
	if err != nil {
		return nil // not a database SQLite can open: the database module reports it
	}
	defer func() { _ = st.Close() }()
	var appID int64
	if err := st.Reader().QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return nil
	}
	if appID == legacyApplicationID {
		return fmt.Errorf("%s is an old Station gateway database; the service keeps its data in its own database. Copy the old data with `llm-gateway gateway-store migrate-from` into a new data directory instead", path)
	}
	return nil
}
