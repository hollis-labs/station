package llmgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/libs/security/secrets"
	"github.com/hollis-labs/station/platform"
	_ "modernc.org/sqlite"
)

const ManagementDocumentVersion = 1
const managementApplicationID = 0x53544757 // STGW
const maxManagementDocument = 4 << 20

var (
	ErrRevisionConflict = errors.New("gateway management revision conflict")
	ErrNoActiveRevision = errors.New("gateway management store has no active revision")
)

// ManagementDocument is the complete desired gateway configuration. Arrays
// retain order: fallbacks and policy rules must never be reordered by storage.
// Credentials are hashes or unresolved secret references, never resolved values.
// Listener, auth mode/JWT and other plugins remain in the bootstrap API file.
type ManagementDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Gateway       Config               `json:"gateway"`
	Clients       []platform.KeyConfig `json:"clients"`
}

func (d ManagementDocument) Validate() error {
	if d.SchemaVersion != ManagementDocumentVersion {
		return fmt.Errorf("unsupported gateway document schema_version %d", d.SchemaVersion)
	}
	if err := d.Gateway.Validate(); err != nil {
		return err
	}
	if err := platform.ValidateClientKeys(d.Clients); err != nil {
		return err
	}
	for _, p := range d.Gateway.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("provider %q needs an HTTP(S) base URL without credentials, query or fragment", p.ID)
		}
		for name := range p.Headers {
			n := strings.ToLower(name)
			if strings.Contains(n, "authorization") || strings.Contains(n, "cookie") || strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(strings.ReplaceAll(n, "-", ""), "apikey") {
				return fmt.Errorf("provider %q: credential headers must use api_key_ref or api_key_env", p.ID)
			}
		}
		if p.APIKeyRef != "" {
			if _, err := secrets.ParseRef(p.APIKeyRef); err != nil {
				return fmt.Errorf("provider %q: invalid secret reference", p.ID)
			}
		}
	}
	if ref := d.Gateway.Ledger.Postgres.DSNRef; ref != "" {
		if _, err := secrets.ParseRef(ref); err != nil {
			return errors.New("invalid ledger secret reference")
		}
	}
	return nil
}

// DecodeManagementDocument rejects unknown fields and trailing documents.
func DecodeManagementDocument(raw []byte) (ManagementDocument, error) {
	var d ManagementDocument
	if len(raw) > maxManagementDocument {
		return d, errors.New("gateway document exceeds 4 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return d, errors.New("expected exactly one gateway document")
	}
	return d, d.Validate()
}

// ManagementRevision is immutable. Activation changes a separate pointer;
// selecting an older revision is an explicit rollback, retained in the audit.
type ManagementRevision struct {
	ID        int64               `json:"id"`
	CreatedAt string              `json:"created_at"`
	Actor     string              `json:"actor"`
	SHA256    string              `json:"sha256"`
	Document  *ManagementDocument `json:"document,omitempty"`
}

type ManagementStatus struct {
	LatestRevision int64 `json:"latest_revision"`
	ActiveRevision int64 `json:"active_revision"`
}

// ManagementStore owns the local desired-state database. Save and Activate
// use immediate transactions and expected revisions to reject lost updates,
// including writes from another process. This is separate from the usage Store.
type ManagementStore struct{ db *sql.DB }

// OpenManagementStore opens or initializes a private database in an existing
// owner-only directory. It refuses ambiguous paths and newer/foreign schemas.
func OpenManagementStore(ctx context.Context, path string) (*ManagementStore, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("gateway database path must be absolute and clean")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("gateway database directory must be owner-only")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		var st os.FileInfo
		st, err = os.Lstat(path)
		if err == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
			err = errors.New("gateway database must be a private regular file")
		}
	}
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &ManagementStore{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *ManagementStore) Close() error { return s.db.Close() }

func (s *ManagementStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var app, version int
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if app != 0 && app != managementApplicationID {
		return errors.New("not a Station gateway database")
	}
	if version > 1 || version < 0 {
		return fmt.Errorf("unsupported gateway database schema %d", version)
	}
	if version == 0 {
		var tables int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("refusing to initialize a nonempty unversioned database")
		}
		_, err = tx.ExecContext(ctx, `
CREATE TABLE gateway_revisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 created_at TEXT NOT NULL, actor TEXT NOT NULL CHECK(length(actor)>0),
 document_version INTEGER NOT NULL CHECK(document_version=1),
 document TEXT NOT NULL CHECK(json_valid(document)), sha256 TEXT NOT NULL
);
CREATE TABLE gateway_state (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 latest_revision INTEGER REFERENCES gateway_revisions(id),
 active_revision INTEGER REFERENCES gateway_revisions(id)
);
INSERT INTO gateway_state(singleton) VALUES(1);
CREATE TABLE gateway_activations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, activated_at TEXT NOT NULL, actor TEXT NOT NULL,
 previous_revision INTEGER REFERENCES gateway_revisions(id),
 revision INTEGER NOT NULL REFERENCES gateway_revisions(id)
);
CREATE TABLE gateway_usage (
 id TEXT PRIMARY KEY NOT NULL, recorded_at INTEGER NOT NULL,
 principal TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
 outcome TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
 cost_usd REAL, entry TEXT NOT NULL CHECK(json_valid(entry))
);
CREATE INDEX gateway_usage_time ON gateway_usage(recorded_at,id);
CREATE INDEX gateway_usage_principal ON gateway_usage(principal,recorded_at);
CREATE INDEX gateway_usage_target ON gateway_usage(provider,model,recorded_at);
PRAGMA application_id=1398032215;
PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
	} else if app != managementApplicationID {
		return errors.New("missing Station gateway database identity")
	}
	return tx.Commit()
}

func (s *ManagementStore) Status(ctx context.Context) (ManagementStatus, error) {
	var st ManagementStatus
	err := s.db.QueryRowContext(ctx, "SELECT coalesce(latest_revision,0), coalesce(active_revision,0) FROM gateway_state WHERE singleton=1").Scan(&st.LatestRevision, &st.ActiveRevision)
	return st, err
}

func validActor(actor string) bool {
	return strings.TrimSpace(actor) != "" && len(actor) <= 256 && !strings.ContainsAny(actor, "\r\n\x00")
}

// Save stages a validated revision. expectedLatest=0 is create-only. Saving
// never activates a revision and never performs network/secret resolution.
func (s *ManagementStore) Save(ctx context.Context, d ManagementDocument, expectedLatest int64, actor string) (ManagementRevision, error) {
	var r ManagementRevision
	if !validActor(actor) {
		return r, errors.New("an actor of 1–256 bytes is required")
	}
	if err := d.Validate(); err != nil {
		return r, err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return r, err
	}
	if len(raw) > maxManagementDocument {
		return r, errors.New("gateway document exceeds 4 MiB")
	}
	hash := sha256.Sum256(raw)
	r = ManagementRevision{CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Actor: actor, SHA256: hex.EncodeToString(hash[:])}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(latest_revision,0) FROM gateway_state WHERE singleton=1").Scan(&current); err != nil {
		return r, err
	}
	if expectedLatest != current {
		return r, ErrRevisionConflict
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO gateway_revisions(created_at,actor,document_version,document,sha256) VALUES(?,?,?,?,?)", r.CreatedAt, actor, d.SchemaVersion, string(raw), r.SHA256)
	if err != nil {
		return r, err
	}
	r.ID, err = result.LastInsertId()
	if err != nil {
		return r, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=? WHERE singleton=1", r.ID); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

type rowScanner interface{ Scan(...any) error }

func readManagementRevision(row rowScanner) (ManagementRevision, error) {
	var r ManagementRevision
	var raw string
	var version int
	if err := row.Scan(&r.ID, &r.CreatedAt, &r.Actor, &version, &raw, &r.SHA256); err != nil {
		return r, err
	}
	hash := sha256.Sum256([]byte(raw))
	if hex.EncodeToString(hash[:]) != r.SHA256 {
		return r, errors.New("gateway revision checksum mismatch")
	}
	d, err := DecodeManagementDocument([]byte(raw))
	if err != nil {
		return r, err
	}
	if d.SchemaVersion != version {
		return r, errors.New("gateway revision version mismatch")
	}
	r.Document = &d
	return r, nil
}

const revisionColumns = "id,created_at,actor,document_version,document,sha256"

func (s *ManagementStore) Revision(ctx context.Context, id int64) (ManagementRevision, error) {
	return readManagementRevision(s.db.QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM gateway_revisions WHERE id=?", id))
}

// Active reads the pointer and revision in one statement so concurrent
// activation cannot mix configuration from different revisions.
func (s *ManagementStore) Active(ctx context.Context) (ManagementRevision, error) {
	r, err := readManagementRevision(s.db.QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM gateway_revisions WHERE id=(SELECT active_revision FROM gateway_state WHERE singleton=1)"))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNoActiveRevision
	}
	return r, err
}

func (s *ManagementStore) Activate(ctx context.Context, id, expectedActive int64, actor string) error {
	if !validActor(actor) {
		return errors.New("an actor of 1–256 bytes is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(active_revision,0) FROM gateway_state WHERE singleton=1").Scan(&current); err != nil {
		return err
	}
	if expectedActive != current {
		return ErrRevisionConflict
	}
	if _, err := readManagementRevision(tx.QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM gateway_revisions WHERE id=?", id)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO gateway_activations(activated_at,actor,previous_revision,revision) VALUES(?,?,nullif(?,0),?)", time.Now().UTC().Format(time.RFC3339Nano), actor, current, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_state SET active_revision=? WHERE singleton=1", id); err != nil {
		return err
	}
	return tx.Commit()
}

// History returns metadata only; callers request a document by revision ID.
func (s *ManagementStore) History(ctx context.Context, before int64, limit int) ([]ManagementRevision, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,created_at,actor,sha256 FROM gateway_revisions WHERE (?=0 OR id<?) ORDER BY id DESC LIMIT ?", before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagementRevision{}
	for rows.Next() {
		var r ManagementRevision
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Actor, &r.SHA256); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ManagementActivation is the durable record of a selection for next startup.
// It deliberately does not claim that a running process has applied it.
type ManagementActivation struct {
	ID               int64  `json:"id"`
	ActivatedAt      string `json:"activated_at"`
	Actor            string `json:"actor"`
	PreviousRevision int64  `json:"previous_revision"`
	Revision         int64  `json:"revision"`
}

func (s *ManagementStore) Activations(ctx context.Context, before int64, limit int) ([]ManagementActivation, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,activated_at,actor,coalesce(previous_revision,0),revision FROM gateway_activations WHERE (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagementActivation{}
	for rows.Next() {
		var a ManagementActivation
		if err := rows.Scan(&a.ID, &a.ActivatedAt, &a.Actor, &a.PreviousRevision, &a.Revision); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
