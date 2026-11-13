package appcmd

import (
	"context"
	"strings"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// IssuedKeyPrincipals is the principal id (`key:<id>`) of every identity key that is not revoked and not
// expired. A unit that never ran identity has no table and no keys.
func IssuedKeyPrincipals(ctx context.Context, st *store.Store) ([]string, error) {
	rows, err := st.Reader().QueryContext(ctx, "SELECT id FROM identity_credentials WHERE revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)", time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
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
