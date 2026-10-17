package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

type ctxKey int

const requestInfoKey ctxKey = 1

// RequestInfo is the per-request record outer middleware (the access log) and
// inner middleware (authentication) share. Outer middleware only sees the
// request it was handed, so facts learned later travel through this pointer.
type RequestInfo struct {
	ID string

	mu        sync.Mutex
	principal string
	verified  bool
}

// SetPrincipal records who the request was authenticated as.
func (ri *RequestInfo) SetPrincipal(id string, verified bool) {
	ri.mu.Lock()
	ri.principal, ri.verified = id, verified
	ri.mu.Unlock()
}

func (ri *RequestInfo) who() (string, bool) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	return ri.principal, ri.verified
}

func infoOf(ctx context.Context) *RequestInfo {
	ri, _ := ctx.Value(requestInfoKey).(*RequestInfo)
	return ri
}

// RequestID returns the request ID the RequestID middleware stamped, or "".
func RequestID(r *http.Request) string {
	if ri := infoOf(r.Context()); ri != nil {
		return ri.ID
	}
	return ""
}

// statusWriter records the status and byte count. It forwards Flush and exposes
// Unwrap so http.ResponseController (and SSE streaming) keep working.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// StockRecover turns a panic in a handler into a 500 without leaking the panic
// value. It observes failures, so it fails open: a panic in the handler is the
// failure it contains.
func StockRecover(logger *slog.Logger) Middleware {
	return Middleware{
		Name: "recover", Phase: PhaseRecover, Failure: FailOpen,
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if v := recover(); v != nil {
						if v == http.ErrAbortHandler {
							panic(v)
						}
						logger.Error("handler panic", "path", r.URL.Path, "request_id", RequestID(r), "panic", v)
						http.Error(w, `{"error":{"code":"internal_error","message":"internal error"}}`, http.StatusInternalServerError)
					}
				}()
				next.ServeHTTP(w, r)
			})
		},
	}
}

// StockRequestID stamps a request ID, honoring a sane inbound X-Request-Id.
func StockRequestID() Middleware {
	return Middleware{
		Name: "request-id", Phase: PhaseTrace, Failure: FailOpen,
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := r.Header.Get("X-Request-Id")
				if id == "" || len(id) > 64 || !printable(id) {
					var b [8]byte
					_, _ = rand.Read(b[:])
					id = hex.EncodeToString(b[:])
				}
				w.Header().Set("X-Request-Id", id)
				ri := &RequestInfo{ID: id}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestInfoKey, ri)))
			})
		},
	}
}

func printable(s string) bool {
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// StockAccessLog logs one structured line per request: who (the verified
// principal once authentication has run), what (method and route pattern, not
// the raw path), and how it went. It never logs headers, query strings or
// bodies, so credentials and prompts cannot reach the log.
func StockAccessLog(logger *slog.Logger, routeOf func(*http.Request) string) Middleware {
	return Middleware{
		Name: "access-log", Phase: PhaseAccessLog, Failure: FailOpen,
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := time.Now()
				sw := &statusWriter{ResponseWriter: w}
				next.ServeHTTP(sw, r)
				if sw.status == 0 {
					sw.status = http.StatusOK
				}
				principal, verified := "", false
				if ri := infoOf(r.Context()); ri != nil {
					principal, verified = ri.who()
				}
				route := ""
				if routeOf != nil {
					route = routeOf(r)
				}
				host, _, _ := net.SplitHostPort(r.RemoteAddr)
				logger.Info("http", "method", r.Method, "route", route, "status", sw.status,
					"bytes", sw.bytes, "dur_ms", time.Since(start).Milliseconds(), "request_id", RequestID(r),
					"principal", principal, "verified", verified, "remote", host)
			})
		},
	}
}

// StockBodyLimit caps request bodies. limitFor returns the cap for a request
// (the matched route's own, else the default). It protects, so it fails closed.
func StockBodyLimit(limitFor func(*http.Request) int64) Middleware {
	return Middleware{
		Name: "body-limit", Phase: PhaseLimits, Failure: FailClosed,
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = http.MaxBytesReader(w, r.Body, limitFor(r))
				next.ServeHTTP(w, r)
			})
		},
	}
}
