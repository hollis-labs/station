package llmgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hollis-labs/libs/security/redact"
)

// RoutePrefix is where the service mounts every gateway route.
const RoutePrefix = "/api/llm-gateway"

// DefaultMaxBody is the request body cap of a route that sets none of its own.
const DefaultMaxBody int64 = 4 << 20

// Host is what the service that hosts the gateway gives it. The gateway owns no listener, no
// authentication and no configuration loading: the host serves Routes behind its own guard.
type Host interface {
	Logger() *slog.Logger
	// ResolveSecret resolves a secret reference. The host adds every value it returns to the
	// redaction scope before the gateway sees it.
	ResolveSecret(ctx context.Context, ref string) (string, error)
	RedactScope() *redact.Scope
}

// Route is one HTTP route the gateway serves.
type Route struct {
	Method string
	// Path is relative to RoutePrefix and starts with "/". It may use net/http pattern syntax.
	Path string
	// Scope is the scope a caller must hold; the host maps the route to it, and a route the host
	// cannot map is refused.
	Scope string
	// MaxBodyBytes caps this route's request body; zero means DefaultMaxBody.
	MaxBodyBytes int64
	// Raw marks a route whose error bodies are the upstream dialect's own (OpenAI or Anthropic
	// shaped) and must reach the client untouched. The framework rewrites error bodies into its
	// flat shape unless the route is exempt (docs/conversion-notes.md, Gap 11), so a host serves
	// Raw routes only through a framework opt-out.
	Raw     bool
	Handler http.Handler
}

// Limited returns the route's handler with its request body cap applied.
func (rt Route) Limited() http.Handler {
	limit := rt.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		rt.Handler.ServeHTTP(w, r)
	})
}

type requestIDKey struct{}

// RequestID returns the id WithRequestID stamped on the request, or "".
func RequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// WithRequestID stamps a request id, honouring a sane inbound X-Request-Id, and echoes it in the
// response header.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 || !printable(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func printable(s string) bool {
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// APIKeyBridge maps Anthropic's x-api-key header to Authorization for the gateway's own routes and
// marks management responses uncacheable. It only moves a header; the host's guard runs next, so a
// request that is not mapped is still refused there. Wrap the host's guard with it.
func APIKeyBridge(next http.Handler) http.Handler {
	prefix := RoutePrefix + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isManagement := strings.HasPrefix(r.URL.Path, prefix+"v1/management/")
		if isManagement {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		if !isManagement && strings.HasPrefix(r.URL.Path, prefix) && r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("X-Api-Key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		r.Header.Del("X-Api-Key")
		next.ServeHTTP(w, r)
	})
}
