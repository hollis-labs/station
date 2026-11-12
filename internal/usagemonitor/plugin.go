package usagemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/substrate/apierr"
)

const (
	ScopeRead    = "usage-monitor.read"
	ScopeManage  = "usage-monitor.manage"
	ScopeIngest  = "usage-monitor.ingest"
	ScopeCollect = "usage-monitor.collect"
)

// RoutePrefix is where the service mounts every usage-monitor route.
const RoutePrefix = "/api/usage-monitor"

// Config is the usage monitor's own block (`usage-monitor:` in the service config). The database
// is the service's (the database module), so there is no database path here.
type Config struct {
	PollInterval string    `yaml:"poll_interval" json:"poll_interval"`
	StaleAfter   string    `yaml:"stale_after" json:"stale_after"`
	Accounts     []Account `yaml:"accounts" json:"accounts"`
	Sources      []Source  `yaml:"sources" json:"sources"`
}

// Host is what the service that hosts the monitor gives it: the logger and credential resolution.
// The monitor owns no listener, no authentication and no configuration loading.
type Host interface {
	Logger() *slog.Logger
	ResolveSecret(ctx context.Context, ref string) (string, error)
}

// Route is one HTTP route the monitor serves.
type Route struct {
	Method string
	// Path is relative to RoutePrefix and starts with "/". It may use net/http pattern syntax.
	Path string
	// Scope is the scope a caller must hold; the host maps the route to it.
	Scope        string
	MaxBodyBytes int64
	Handler      http.Handler
}

// Limited returns the route's handler with its request body cap applied.
func (rt Route) Limited() http.Handler {
	limit := rt.MaxBodyBytes
	if limit <= 0 {
		limit = maxResponse
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		rt.Handler.ServeHTTP(w, r)
	})
}

// Options are what the host passes besides the configuration.
type Options struct {
	// Store is the service database with the usage schema applied (Migrations).
	Store *store.Store
	// Transport is the HTTP transport of the collectors; nil uses the default.
	Transport http.RoundTripper
}

// Service is the running monitor: the routes to serve and the collectors to run.
type Service struct {
	routes     []Route
	collectors map[string]*collector
	interval   time.Duration
	logger     *slog.Logger
}

// Routes are the HTTP routes to mount under RoutePrefix.
func (s *Service) Routes() []Route { return s.routes }

// Start validates the configuration, records the bootstrap accounts and builds the routes and
// collectors. It starts nothing: Run starts the collectors, so `validate` opens and checks storage
// without fetching accounts or launching agy.
func Start(ctx context.Context, host Host, cfg Config, o Options) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("usage-monitor: a database is required")
	}
	interval, err := duration(cfg.PollInterval, 5*time.Minute)
	if err != nil || interval < time.Minute || interval > 24*time.Hour {
		return nil, errors.New("poll_interval must be between 1m and 24h")
	}
	stale, err := duration(cfg.StaleAfter, 3*interval)
	if err != nil || stale < interval {
		return nil, errors.New("stale_after must be at least poll_interval")
	}
	accounts := map[string]Account{}
	for _, a := range cfg.Accounts {
		if err = a.validate(); err != nil {
			return nil, err
		}
		if _, ok := accounts[a.ID]; ok {
			return nil, errors.New("duplicate bootstrap account")
		}
		accounts[a.ID] = a
	}
	sources := map[string]Source{}
	for _, s := range cfg.Sources {
		if err = s.validate(); err != nil {
			return nil, fmt.Errorf("source %s: %w", s.ID, err)
		}
		if _, ok := sources[s.ID]; ok {
			return nil, errors.New("duplicate source")
		}
		sources[s.ID] = s
	}
	store := NewStore(o.Store)
	for _, a := range cfg.Accounts {
		old, e := store.Account(ctx, a.ID)
		if errors.Is(e, ErrNotFound) {
			if _, err = store.PutAccount(ctx, a, 0, "bootstrap"); err != nil {
				return nil, err
			}
		} else if e != nil {
			return nil, e
		} else if old.Provider != a.Provider || old.Kind != a.Kind {
			return nil, errors.New("bootstrap account identity conflicts with stored account")
		}
	}
	api := &api{store: store, sources: sources, collectors: map[string]*collector{}, stale: stale}
	for _, s := range cfg.Sources {
		a, e := store.Account(ctx, s.AccountID)
		if e != nil {
			return nil, fmt.Errorf("source %s account: %w", s.ID, e)
		}
		expected := map[string]string{"claude": "anthropic", "codex": "openai", "opencode-go": "opencode-go", "agy": "antigravity"}[s.Collector]
		if expected != "" && (a.Provider != expected || a.Kind != "subscription") {
			return nil, fmt.Errorf("source %s requires a %s subscription account", s.ID, expected)
		}
		if s.Collector == "push" {
			continue
		}
		api.collectors[s.ID] = &collector{source: s, store: store, resolve: host.ResolveSecret, client: &http.Client{Transport: o.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	svc := &Service{collectors: api.collectors, interval: interval, logger: host.Logger()}
	for _, rt := range []Route{
		{Method: "GET", Path: "/v1/accounts", Scope: ScopeRead, Handler: http.HandlerFunc(api.accounts)},
		{Method: "PUT", Path: "/v1/accounts/{id}", Scope: ScopeManage, Handler: http.HandlerFunc(api.putAccount)},
		{Method: "GET", Path: "/v1/sources", Scope: ScopeRead, Handler: http.HandlerFunc(api.listSources)},
		{Method: "GET", Path: "/v1/observations", Scope: ScopeRead, Handler: http.HandlerFunc(api.observations)},
		{Method: "GET", Path: "/v1/events", Scope: ScopeRead, Handler: http.HandlerFunc(api.events)},
		{Method: "GET", Path: "/v1/summary", Scope: ScopeRead, Handler: http.HandlerFunc(api.summary)},
		{Method: "POST", Path: "/v1/sources/{id}/ingest", Scope: ScopeIngest, Handler: http.HandlerFunc(api.ingest)},
		{Method: "POST", Path: "/v1/sources/{id}/collect", Scope: ScopeCollect, Handler: http.HandlerFunc(api.collect)},
	} {
		rt.MaxBodyBytes = maxResponse
		rt.Handler = authorized(rt.Scope, rt.Handler)
		svc.routes = append(svc.routes, rt)
	}
	return svc, nil
}

func duration(v string, fallback time.Duration) (time.Duration, error) {
	if v == "" {
		return fallback, nil
	}
	return time.ParseDuration(v)
}

// Run polls every collector on the interval until ctx ends, and returns once each has stopped. A
// push-only configuration has no collectors and simply waits for ctx. The host runs it beside the
// HTTP server and waits for it before the database closes.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for id, c := range s.collectors {
		wg.Go(func() {
			timer := time.NewTimer(0)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					if err := c.Collect(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrDisabled) {
						s.logger.Warn("collection unsuccessful", "source", id)
					}
					timer.Reset(s.interval)
				}
			}
		})
	}
	wg.Wait()
	<-ctx.Done()
}

type api struct {
	store      *Store
	sources    map[string]Source
	collectors map[string]*collector
	stale      time.Duration
}

func authorized(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		p, ok := auth.FromContext(r.Context())
		_, state := auth.ParseBearer(r.Header.Values("Authorization"))
		if !ok || !p.Verified || state != "" {
			problem(w, 401, "unauthenticated")
			return
		}
		if !p.HasScope(scope) {
			problem(w, 403, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		problem(w, http.StatusInternalServerError, "encoding_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

// problem answers in the framework's flat error shape: {code, message}.
func problem(w http.ResponseWriter, status int, code string) {
	apierr.Write(w, status, code, strings.ReplaceAll(code, "_", " "), nil)
}
func decodeJSON(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		problem(w, 404, "not_found")
	case errors.Is(err, ErrConflict):
		problem(w, 409, "conflict")
	case errors.Is(err, ErrDisabled):
		problem(w, 409, "account_disabled")
	default:
		problem(w, 500, "storage_error")
	}
}

func (a *api) accounts(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.Accounts(r.Context(), a.stale)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"accounts": v})
}
func (a *api) listSources(w http.ResponseWriter, r *http.Request) {
	type view struct {
		Source
		Latest *Record[Observation] `json:"latest_attempt"`
		Stale  bool                 `json:"stale"`
	}
	out := []view{}
	for _, s := range a.sources {
		latest, err := a.store.latest(r.Context(), s.AccountID, s.ID, false)
		if err != nil {
			storeError(w, err)
			return
		}
		stale := latest == nil || time.Since(latest.Data.ObservedAt) > a.stale || latest.Data.Status == "error"
		out = append(out, view{Source: s, Latest: latest, Stale: stale})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"sources": out})
}
func (a *api) putAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account          Account `json:"account"`
		ExpectedRevision *int64  `json:"expected_revision"`
	}
	if decodeJSON(r, &in) != nil || in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || in.Account.ID != r.PathValue("id") || in.Account.validate() != nil {
		problem(w, 400, "invalid_account")
		return
	}
	p, _ := auth.FromContext(r.Context())
	v, err := a.store.PutAccount(r.Context(), in.Account, *in.ExpectedRevision, p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func parseQuery(r *http.Request) (Query, error) {
	v := r.URL.Query()
	q := Query{Account: v.Get("account_id"), Source: v.Get("source"), Limit: 100}
	var err error
	for k := range v {
		switch k {
		case "account_id", "source", "after", "limit", "since", "until":
		default:
			return q, errors.New("unknown query parameter")
		}
		if len(v[k]) != 1 {
			return q, errors.New("duplicate query parameter")
		}
	}
	if v.Has("after") {
		q.After, err = strconv.ParseInt(v.Get("after"), 10, 64)
		if err != nil || q.After < 0 {
			return q, errors.New("invalid cursor")
		}
	}
	if v.Has("limit") {
		q.Limit, err = strconv.Atoi(v.Get("limit"))
		if err != nil || q.Limit < 1 || q.Limit > 500 {
			return q, errors.New("invalid limit")
		}
	}
	if v.Has("since") {
		q.Since, err = time.Parse(time.RFC3339Nano, v.Get("since"))
		if err != nil {
			return q, err
		}
	}
	if v.Has("until") {
		q.Until, err = time.Parse(time.RFC3339Nano, v.Get("until"))
		if err != nil {
			return q, err
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Since.Before(q.Until) {
		return q, errors.New("invalid time range")
	}
	return q, nil
}
func (a *api) observations(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		problem(w, 400, "invalid_query")
		return
	}
	v, err := a.store.Observations(r.Context(), q)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (a *api) events(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		problem(w, 400, "invalid_query")
		return
	}
	v, err := a.store.Events(r.Context(), q)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (a *api) summary(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil || q.After != 0 {
		problem(w, 400, "invalid_query")
		return
	}
	v, err := a.store.Totals(r.Context(), q)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"totals": v})
}
func (a *api) ingest(w http.ResponseWriter, r *http.Request) {
	s, ok := a.sources[r.PathValue("id")]
	if !ok {
		problem(w, 404, "not_found")
		return
	}
	p, _ := auth.FromContext(r.Context())
	if s.Collector != "push" || s.Principal != p.ID {
		problem(w, 403, "source_forbidden")
		return
	}
	var b Batch
	if decodeJSON(r, &b) != nil {
		problem(w, 400, "invalid_batch")
		return
	}
	account, err := a.store.Account(r.Context(), s.AccountID)
	if err != nil {
		storeError(w, err)
		return
	}
	if err = b.validate(account); err != nil {
		problem(w, 400, "invalid_batch")
		return
	}
	if err = a.store.Ingest(r.Context(), s.AccountID, s.ID, p.ID, b); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"committed": true})
}
func (a *api) collect(w http.ResponseWriter, r *http.Request) {
	c, ok := a.collectors[r.PathValue("id")]
	if !ok {
		problem(w, 404, "collector_not_found")
		return
	}
	if err := c.Collect(r.Context()); err != nil {
		if errors.Is(err, ErrDisabled) {
			storeError(w, err)
		} else {
			problem(w, 502, "collection_failed")
		}
		return
	}
	writeJSON(w, 200, map[string]bool{"collected": true})
}
