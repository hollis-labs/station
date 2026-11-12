package main

import (
	"context"
	"log/slog"

	"github.com/hollis-labs/libs/security/redact"
	libsecrets "github.com/hollis-labs/libs/security/secrets"
)

// secretHost is the gateway's view of the service: the framework logger, the secrets resolver, and
// a redaction scope of its own for the values it resolves (the capture sanitizer uses it). The
// framework's own scope already covers logs: the secrets module registers every resolved value.
type secretHost struct {
	log   *slog.Logger
	res   *libsecrets.Resolver
	scope *redact.Scope
}

func newHost(log *slog.Logger, res *libsecrets.Resolver) *secretHost {
	return &secretHost{log: log, res: res, scope: redact.NewScope()}
}

func (h *secretHost) Logger() *slog.Logger { return h.log }

func (h *secretHost) RedactScope() *redact.Scope { return h.scope }

func (h *secretHost) ResolveSecret(ctx context.Context, ref string) (string, error) {
	r, err := h.res.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	v := r.Secret.Reveal()
	h.scope.Add("resolved-secret", v)
	return v, nil
}
