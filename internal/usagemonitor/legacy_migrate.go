package usagemonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/hollis-labs/libs/core/store"
)

// The old usage database (station's usage.db) is identified by this SQLite application id and
// user_version. Its four tables are the ones the service database has, column for column.
const (
	legacyApplicationID = 0x5354554d
	legacyUserVersion   = 1
)

// legacyTables is every table in copy order (parents before the tables that reference them), with the
// column that orders its rows.
var legacyTables = []struct{ name, order string }{
	{"accounts", "id"},
	{"account_audit", "sequence"},
	{"observations", "sequence"},
	{"events", "sequence"},
}

// LegacyReport is what MigrateLegacy copied: the row count of each table, a digest of every table's
// rows (equal on both sides, or nothing is written), and the integrity check of both files.
type LegacyReport struct {
	Rows      map[string]int64  `json:"rows"`
	Digests   map[string]string `json:"digests"`
	Integrity string            `json:"integrity_check"`
}

// MigrateLegacy copies an old usage.db into the (empty) service database in one transaction, keeping
// every sequence, then verifies a digest of each table's rows against the old file before the commit,
// so on any error nothing is written to dst. Run it with the old unit stopped (usage is written
// continuously: copying a live file would lose what arrives after the copy) and on an online-backup
// copy of the file: opening a file moves it to WAL and may create sidecar files.
func MigrateLegacy(ctx context.Context, dst *store.Store, legacyPath string) (LegacyReport, error) {
	rep := LegacyReport{Rows: map[string]int64{}, Digests: map[string]string{}}
	if _, err := os.Stat(legacyPath); err != nil {
		return rep, err
	}
	old, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: legacyPath, ReadConns: 1}})
	if err != nil {
		return rep, fmt.Errorf("open the old usage database: %w", err)
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
		return rep, fmt.Errorf("%s is not an old usage database (application_id %#x, user_version %d)", legacyPath, appID, userVersion)
	}
	var check string
	if err := old.Reader().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return rep, err
	}
	if check != "ok" {
		return rep, fmt.Errorf("the old usage database fails its integrity check: %s", check)
	}
	for _, t := range legacyTables {
		var n int64
		if err := dst.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.name).Scan(&n); err != nil {
			return rep, err
		}
		if n != 0 {
			return rep, fmt.Errorf("the service database already has rows in %s: migrate into a new, empty database", t.name)
		}
	}

	// The callback only reads the old file and inserts into dst, so a restart after contention
	// repeats it from the start with nothing half-done.
	err = dst.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		rep.Rows, rep.Digests = map[string]int64{}, map[string]string{}
		for _, t := range legacyTables {
			n, err := copyTable(ctx, old.Reader(), tx, t.name, t.order)
			if err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
			rep.Rows[t.name] = n
			want, err := digest(ctx, old.Reader(), t.name, t.order)
			if err != nil {
				return err
			}
			got, err := digest(ctx, tx, t.name, t.order)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("%s: the copied rows do not match the old file", t.name)
			}
			rep.Digests[t.name] = got
		}
		return nil
	})
	if err != nil {
		return LegacyReport{}, fmt.Errorf("copy the old usage database: %w", err)
	}
	if err := dst.Reader().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&rep.Integrity); err != nil {
		return rep, err
	}
	if rep.Integrity != "ok" {
		return rep, fmt.Errorf("the service database fails its integrity check after the copy: %s", rep.Integrity)
	}
	return rep, nil
}

// copyTable copies every column of table, in order, from src into tx.
func copyTable(ctx context.Context, src, tx store.Querier, table, order string) (int64, error) {
	rows, err := src.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	insert := "INSERT INTO " + table + " VALUES(?" + repeat(",?", len(cols)-1) + ")"
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		if _, err := tx.ExecContext(ctx, insert, vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// digest is a SHA-256 over every value of every row of table, in order, with its type.
func digest(ctx context.Context, q store.Querier, table, order string) (string, error) {
	rows, err := q.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+order)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	h := sha256.New()
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, v := range vals {
			_, _ = fmt.Fprintf(h, "%T:%v\x1f", v, v)
		}
		_, _ = h.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RefuseLegacyDatabase is a start guard: it returns an error when path is an old usage.db, so a
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
		return fmt.Errorf("%s is an old Station usage database; the service keeps its data in its own database. Copy the old data with `usage-monitor migrate-from` into a new data directory instead", path)
	}
	return nil
}
