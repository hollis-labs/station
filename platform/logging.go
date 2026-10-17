package platform

import (
	"io"
	"log/slog"

	"github.com/hollis-labs/libs/security/redact"
)

// Logging is Station's logger plus the redaction scope behind it.
//
// Every record passes through the redactor before it is written: messages and
// string, error and stringer attributes are scanned for credential shapes, and
// an attribute with a sensitive key loses its value. The Scope holds secrets
// the process KNOWS (a resolved provider key); it matches them exactly, in
// their encoded forms too, which is stronger than shape matching. Secret
// resolution feeds the scope through Sink, so a key is protected from the
// moment it is resolved.
//
// This is a safety net, not permission to log sensitive data: the gateway
// never logs prompts, completions, request bodies or headers in the first place.
type Logging struct {
	Logger *slog.Logger
	Scope  *redact.Scope
}

// NewLogging builds a redacting logger writing to w. format is "json" or
// "text"; level is the minimum level.
func NewLogging(w io.Writer, format string, level slog.Level) (*Logging, error) {
	scope := redact.NewScope()
	red, err := redact.New(redact.WithScope(scope))
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return &Logging{Logger: slog.New(red.Handler(h)), Scope: scope}, nil
}

// Sink adapts the scope to the secrets resolver's ValueSink.
func (l *Logging) Sink() ValueSink { return scopeSink{l.Scope} }

// ValueSink is the resolver's callback: it learns every value resolved.
type ValueSink interface{ Add(values ...string) }

type scopeSink struct{ s *redact.Scope }

func (k scopeSink) Add(values ...string) {
	for _, v := range values {
		k.s.Add("resolved-secret", v)
	}
}
