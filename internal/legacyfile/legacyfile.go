// Package legacyfile reads the old units' SQLite files without altering them.
//
// Opening a SQLite file read-write switches it to WAL, may create -wal and -shm sidecars and can
// rewrite its header, so the old files (which the cutover must leave exactly as they were, for
// rollback) are never opened through SQLite. Take copies one aside and the copy is what gets opened;
// ApplicationID answers "what kind of file is this" from the file header, and falls back to a copy
// only when an uncheckpointed -wal sidecar could hold a newer header.
package legacyfile

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hollis-labs/libs/core/store"
)

const (
	magic              = "SQLite format 3\x00"
	headerSize         = 100
	offUserVersion     = 60
	offApplicationID   = 68
	sidecarWAL         = "-wal"
	privateDirMode     = 0o700
	privateFileMode    = 0o600
	hashDomainDatabase = "db\n"
	hashDomainWAL      = "\nwal\n"
)

// Snapshot is a private copy of an SQLite file and its -wal sidecar, taken so nothing opens the
// original through SQLite.
type Snapshot struct {
	// Path is the copy; open this one.
	Path string
	// SHA256 is the sha256 of the source database file (not its -wal) when it was copied.
	SHA256 string

	src string
	dir string
	sum string // digest of the database and its -wal, for Unchanged
}

// Take copies src (and src-wal, when present) into a new private directory. It fails if the source
// changed while it was copied, which means the old unit is still writing to it.
func Take(src string) (*Snapshot, error) {
	before, err := digest(src)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "station-legacy-")
	if err != nil {
		return nil, err
	}
	plain, err := fileSHA256(src)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &Snapshot{src: src, dir: dir, Path: filepath.Join(dir, "copy.db"), SHA256: plain, sum: before}
	if err = copyFile(src, s.Path); err == nil {
		if _, e := os.Stat(src + sidecarWAL); e == nil {
			err = copyFile(src+sidecarWAL, s.Path+sidecarWAL)
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	if after, e := digest(src); e != nil || after != before {
		s.Close()
		return nil, errors.New("the old file changed while it was copied: stop the old unit and copy a backup of it")
	}
	return s, nil
}

// Unchanged reports whether the source is byte for byte what it was when it was copied.
func (s *Snapshot) Unchanged() (bool, error) {
	now, err := digest(s.src)
	return err == nil && now == s.sum, err
}

// Close removes the copy.
func (s *Snapshot) Close() { _ = os.RemoveAll(s.dir) }

// ApplicationID returns the application_id of the SQLite file at path. known is false for a missing
// or empty file and for one that is not SQLite. The file is read as bytes; only when an uncheckpointed
// -wal sidecar exists (which can hold a newer header) is a private copy opened through SQLite. The
// original is never opened by SQLite.
func ApplicationID(ctx context.Context, path string) (id int64, known bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, headerSize)
	n, err := io.ReadFull(f, hdr)
	if err != nil && n < headerSize {
		return 0, false, nil // empty or too short to be a database
	}
	if string(hdr[:len(magic)]) != magic {
		return 0, false, nil
	}
	if info, e := os.Stat(path + sidecarWAL); e != nil || info.Size() == 0 {
		return int64(binary.BigEndian.Uint32(hdr[offApplicationID:])), true, nil
	}
	snap, err := Take(path)
	if err != nil {
		return 0, false, err
	}
	defer snap.Close()
	st, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: snap.Path, ReadConns: 1}})
	if err != nil {
		return 0, false, nil // not a database SQLite can open: the database module reports it
	}
	defer func() { _ = st.Close() }()
	if err = st.Reader().QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
		return 0, false, nil
	}
	return id, true, nil
}

// UserVersion is the user_version in the header of the file at path (no sidecar is considered).
func UserVersion(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, headerSize)
	if _, err = io.ReadFull(f, hdr); err != nil || string(hdr[:len(magic)]) != magic {
		return 0, fmt.Errorf("%s is not an SQLite database", path)
	}
	return int64(binary.BigEndian.Uint32(hdr[offUserVersion:])), nil
}

func digest(src string) (string, error) {
	h := sha256.New()
	add := func(domain, p string, must bool) error {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) && !must {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, _ = h.Write([]byte(domain))
		_, err = io.Copy(h, f)
		return err
	}
	if err := add(hashDomainDatabase, src, true); err != nil {
		return "", err
	}
	if err := add(hashDomainWAL, src+sidecarWAL, false); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err = os.MkdirAll(filepath.Dir(to), privateDirMode); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
