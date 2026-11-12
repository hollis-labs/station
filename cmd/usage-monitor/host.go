package main

import (
	"context"
	"log/slog"

	libsecrets "github.com/hollis-labs/libs/security/secrets"
)

// secretHost is the monitor's view of the service: the framework logger and the secrets resolver
// (the secrets module already registers every resolved value with the redaction scope of the logs).
type secretHost struct {
	log *slog.Logger
	res *libsecrets.Resolver
}

func (h secretHost) Logger() *slog.Logger { return h.log }

func (h secretHost) ResolveSecret(ctx context.Context, ref string) (string, error) {
	r, err := h.res.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return r.Secret.Reveal(), nil
}
