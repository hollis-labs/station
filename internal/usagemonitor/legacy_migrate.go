package usagemonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/station/internal/legacyfile"
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
	// SourceSHA256 is the digest of the old file (and its -wal) taken before it was copied;
	// SourceUnchanged says it still matched at the end. The old file is never opened by SQLite.
	SourceSHA256    string `json:"source_sha256"`
	SourceUnchanged bool   `json:"source_unchanged"`
}

// MigrateLegacy copies an old usage.db into the (empty) service database in one transaction, keeping
// every sequence, then verifies a digest of each table's rows against the old file before the commit,
// so on any error nothing is written to dst. Run it with the old unit stopped (usage is written
// continuously: copying a live file would lose what arrives after the copy) and on an online-backup
// copy of the file as well: the file is only ever read as bytes (a private copy is what SQLite opens),
// and the report carries its sha256 and whether it still matched at the end.
func MigrateLegacy(ctx context.Context, dst *store.Store, legacyPath string) (LegacyReport, error) {
	rep := LegacyReport{Rows: map[string]int64{}, Digests: map[string]string{}}
	if _, err := os.Stat(legacyPath); err != nil {
		return rep, err
	}
	// SQLite never opens the old file: a read-write open switches it to WAL and may create sidecars.
	// A private copy is opened instead, and the source's digest is proved unchanged at the end.
	snap, err := legacyfile.Take(legacyPath)
	if err != nil {
		return rep, fmt.Errorf("copy the old usage database aside: %w", err)
	}
	defer snap.Close()
	rep.SourceSHA256 = snap.SHA256
	old, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: snap.Path, ReadConns: 1}})
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
	rep.SourceSHA256 = snap.SHA256
	if same, e := snap.Unchanged(); e != nil || !same {
		return rep, errors.New("the old file changed during the migration: discard the new database and copy a fresh backup")
	}
	rep.SourceUnchanged = true
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
// read from the file header by legacyfile, which never opens the file through SQLite (that would
// switch it to WAL): only an uncheckpointed -wal sidecar, which can hold a newer header, makes it
// read a private copy.
func RefuseLegacyDatabase(path string) error {
	id, known, err := legacyfile.ApplicationID(context.Background(), path)
	if err != nil {
		return err
	}
	if known && id == legacyApplicationID {
		return fmt.Errorf("%s is an old Station usage database; the service keeps its data in its own database. Copy the old data with `usage-monitor migrate-from` into a new data directory instead", path)
	}
	return nil
}
