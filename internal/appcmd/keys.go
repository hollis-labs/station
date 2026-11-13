package appcmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// IssuedKeyPrincipals is the principal id (`key:<id>`) of every identity key that is not revoked and not
// expired. It reads the identity module's credentials table, which has no listing API. A unit that never
// ran identity has neither identity table and no keys; any other shape (one table present, the other
// missing, a changed column) is an error, never "no keys".
func IssuedKeyPrincipals(ctx context.Context, st *store.Store) ([]string, error) {
	has := func(name string) (bool, error) {
		var n int
		err := st.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
		return n > 0, err
	}
	creds, err := has("identity_credentials")
	if err != nil {
		return nil, err
	}
	if !creds {
		users, err := has("identity_users")
		if err != nil {
			return nil, err
		}
		if users {
			return nil, errors.New("the identity module's credentials table is missing: cannot list the issued keys")
		}
		return nil, nil
	}
	rows, err := st.Reader().QueryContext(ctx, "SELECT id FROM identity_credentials WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)", time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list the issued keys (the identity module's table may have changed): %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, "key:"+id)
	}
	return out, rows.Err()
}
