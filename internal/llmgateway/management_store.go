package llmgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/security/secrets"
)

// Migrations is the gateway's schema, one squashed goose migration for the database module's SQLite
// (register it with database.UseMigrationFS(Migrations, "migrations") before boot).
//
//go:embed migrations
var Migrations embed.FS

const ManagementDocumentVersion = 1
const maxManagementDocument = 4 << 20

var (
	ErrRevisionConflict = errors.New("gateway management revision conflict")
	ErrNoActiveRevision = errors.New("gateway management store has no active revision")
)

// ManagementDocument is the complete desired gateway configuration. Arrays
// retain order: fallbacks and policy rules must never be reordered by storage.
// Credentials are unresolved secret references, never resolved values. Client keys are not part of
// the document: the identity module issues them. Clients exists only so that a revision stored by the
// old gateway, which carried hashed client keys, still decodes and keeps its checksum; a new document
// that names clients is refused.
type ManagementDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Gateway       Config          `json:"gateway"`
	Clients       json.RawMessage `json:"clients,omitempty"`
}

func (d ManagementDocument) Validate() error {
	if d.SchemaVersion != ManagementDocumentVersion {
		return fmt.Errorf("unsupported gateway document schema_version %d", d.SchemaVersion)
	}
	if err := d.Gateway.Validate(); err != nil {
		return err
	}
	if c := bytes.TrimSpace(d.Clients); len(c) > 0 && string(c) != "null" && string(c) != "[]" {
		return errors.New("client keys are issued by the identity module: remove the clients list from the document")
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
	if ref := d.Gateway.Ledger.Postgres.PasswordRef; ref != "" {
		if _, err := secrets.ParseRef(ref); err != nil {
			return errors.New("invalid ledger password reference")
		}
	}
	return nil
}

// DecodeManagementDocument rejects unknown fields and trailing documents.
func DecodeManagementDocument(raw []byte) (ManagementDocument, error) {
	d, err := decodeManagementDocument(raw)
	if err != nil {
		return d, err
	}
	return d, d.Validate()
}

// decodeManagementDocument decodes without validating, for revisions already stored.
func decodeManagementDocument(raw []byte) (ManagementDocument, error) {
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
	return d, nil
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

// ManagementStore owns the desired-state tables in the database module's store. Writes use WriteTx
// (one write connection, restarted on contention, so each callback is safe to run again) and
// expected revisions reject lost updates, including writes from another process; reads use the
// read pool, never a statement nested inside an open cursor.
type ManagementStore struct{ st *store.Store }

// NewManagementStore wraps the database module's store. The schema is Migrations, applied by the module.
func NewManagementStore(st *store.Store) *ManagementStore { return &ManagementStore{st: st} }

func (s *ManagementStore) Status(ctx context.Context) (ManagementStatus, error) {
	var st ManagementStatus
	err := s.st.Reader().QueryRowContext(ctx, "SELECT coalesce(latest_revision,0), coalesce(active_revision,0) FROM gateway_state WHERE singleton=1").Scan(&st.LatestRevision, &st.ActiveRevision)
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
	err = s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(latest_revision,0) FROM gateway_state WHERE singleton=1").Scan(&current); err != nil {
			return err
		}
		if expectedLatest != current {
			return ErrRevisionConflict
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO gateway_revisions(created_at,actor,document_version,document,sha256) VALUES(?,?,?,?,?)", r.CreatedAt, actor, d.SchemaVersion, string(raw), r.SHA256)
		if err != nil {
			return err
		}
		if r.ID, err = result.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE gateway_state SET latest_revision=? WHERE singleton=1", r.ID)
		return err
	})
	return r, err
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
	// A stored revision decodes as written, including the hashed client list an old revision carries.
	d, err := decodeManagementDocument([]byte(raw))
	if err != nil {
		return r, err
	}
	if err = d.Gateway.Validate(); err != nil {
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
	return readManagementRevision(s.st.Reader().QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM gateway_revisions WHERE id=?", id))
}

// Active reads the pointer and revision in one statement so concurrent
// activation cannot mix configuration from different revisions.
func (s *ManagementStore) Active(ctx context.Context) (ManagementRevision, error) {
	r, err := readManagementRevision(s.st.Reader().QueryRowContext(ctx, "SELECT "+revisionColumns+" FROM gateway_revisions WHERE id=(SELECT active_revision FROM gateway_state WHERE singleton=1)"))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNoActiveRevision
	}
	return r, err
}

func (s *ManagementStore) Activate(ctx context.Context, id, expectedActive int64, actor string) error {
	if !validActor(actor) {
		return errors.New("an actor of 1–256 bytes is required")
	}
	return s.st.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
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
		_, err := tx.ExecContext(ctx, "UPDATE gateway_state SET active_revision=? WHERE singleton=1", id)
		return err
	})
}

// History returns metadata only; callers request a document by revision ID.
func (s *ManagementStore) History(ctx context.Context, before int64, limit int) ([]ManagementRevision, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.st.Reader().QueryContext(ctx, "SELECT id,created_at,actor,sha256 FROM gateway_revisions WHERE (?=0 OR id<?) ORDER BY id DESC LIMIT ?", before, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	rows, err := s.st.Reader().QueryContext(ctx, `SELECT id,activated_at,actor,coalesce(previous_revision,0),revision FROM gateway_activations WHERE (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
