package llmgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/libs/llm/core/guard"
	"github.com/hollis-labs/libs/llm/core/llmtypes"
	"github.com/hollis-labs/libs/llm/core/usageledger"
	"github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/libs/security/redact"

	"github.com/hollis-labs/station/platform"
)

const (
	maxMeteredBody = 8 << 20 // beyond this a non-streaming body is relayed but not metered
	maxSSELine     = 1 << 20
)

type provider struct {
	id              string
	catalogProvider string
	dialect         Dialect
	baseURL         string
	headers         map[string]string
	opencodeSession bool
	apiKey          string
	client          *http.Client
}

type target struct {
	reservation   *Entry // set only on a per-attempt copy, never a shared route
	provider      *provider
	model         string
	operations    []Operation
	translate     bool
	tags          []string
	bestEffort    bool
	headerTimeout time.Duration
	capsCfg       CapabilitiesConfig
	caps          llmtypes.ProviderCapabilities
	price         *usageledger.PriceSnapshot
}

// Gateway relays provider-native requests to configured upstreams.
type Gateway struct {
	models          map[string][]*target
	kinds           map[string]Operation
	names           []string
	guard           *guard.Guard
	bestEffortGuard *guard.Guard
	ledger          *Ledger
	budgets         *Budgets
	catalog         *Catalog
	capturer        *Capturer
	gate            *gate
	logger          *slog.Logger
}

// NewGateway builds a Gateway from validated configuration. providers maps a
// provider ID to its runtime (dialect, credential, HTTP client).
func NewGateway(cfg Config, providers map[string]*provider, ledger *Ledger, logger *slog.Logger, scope ...*redact.Scope) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	var store Store
	if ledger != nil {
		store = ledger.store
	}
	budgets, err := NewBudgets(cfg.Budgets, store, nil)
	if err != nil {
		return nil, err
	}
	catalog, err := NewCatalog(cfg.Catalog, nil, logger)
	if err != nil {
		return nil, err
	}
	var rscope *redact.Scope
	if len(scope) > 0 {
		rscope = scope[0]
	}
	if rscope == nil {
		rscope = redact.NewScope()
	}
	for _, p := range providers {
		if p != nil && p.apiKey != "" {
			rscope.Add("provider-"+p.id, p.apiKey)
		}
	}
	capturer, err := NewCapturer(cfg.Ledger.Capture, rscope)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		models: map[string][]*target{},
		kinds:  map[string]Operation{},
		guard:  guard.New(guard.Config{}),
		// Best-effort routes (local/tailnet llama servers that may sleep or
		// reboot) trip their breaker on the first failure so subsequent hub
		// calls skip them with zero wait until cooldown expires.
		bestEffortGuard: guard.New(guard.Config{
			Breaker: guard.BreakerConfig{Threshold: 1, Cooldown: 30 * time.Second},
		}),
		ledger:   ledger,
		budgets:  budgets,
		catalog:  catalog,
		capturer: capturer,
		gate:     newGate(cfg.Policy, logger),
		logger:   logger,
	}
	disabledProviders := map[string]bool{}
	for _, pc := range cfg.Providers {
		disabledProviders[pc.ID] = !enabled(pc.Enabled)
	}
	for _, m := range cfg.Models {
		if !enabled(m.Enabled) {
			continue
		}
		g.kinds[m.Name], _ = m.operation()
		for _, t := range m.Targets {
			if disabledProviders[t.Provider] {
				continue
			}
			p, ok := providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("llm-gateway: no runtime for provider %q", t.Provider)
			}
			g.models[m.Name] = append(g.models[m.Name], &target{
				provider:      p,
				model:         t.Model,
				translate:     t.Translate,
				tags:          slices.Clone(t.Tags),
				bestEffort:    t.BestEffort,
				headerTimeout: t.headerTimeout(),
				capsCfg:       t.Capabilities,
				operations:    t.Operations,
				caps:          t.Capabilities.resolve(),
				price:         snapshot(t.Price),
			})
		}
		if len(g.models[m.Name]) > 0 {
			g.names = append(g.names, m.Name)
		}
	}
	return g, nil
}

// Catalog returns the gateway's centralized model catalog.
func (g *Gateway) Catalog() *Catalog { return g.catalog }

// SetCatalog replaces the gateway's model catalog (for example when wiring a
// custom HTTP transport from the plugin host).
func (g *Gateway) SetCatalog(c *Catalog) { g.catalog = c }

func (g *Gateway) guardFor(t *target) *guard.Guard {
	if t != nil && t.bestEffort {
		return g.bestEffortGuard
	}
	return g.guard
}

func (g *Gateway) record(rec Record) {
	if g.ledger == nil {
		return
	}
	e := g.ledger.Add(rec)
	if g.budgets != nil && rec.Target.reservation == nil {
		if err := g.budgets.Record(e); err != nil {
			g.logger.Error("budget.record_failed", "request_id", rec.RequestID)
		}
	}
}

func snapshot(p *PriceConfig) *usageledger.PriceSnapshot {
	if p == nil {
		return nil
	}
	return &usageledger.PriceSnapshot{
		InputPerMillion:      p.InputPerMillion,
		OutputPerMillion:     p.OutputPerMillion,
		CacheReadPerMillion:  p.CacheReadPerMillion,
		CacheWritePerMillion: p.CacheWritePerMillion,
		ReasoningPerMillion:  p.ReasoningPerMillion,
		Source:               "llm-gateway-config",
		// A rate left at 0 means "not configured", and an unmarked 0 would
		// read as free, so mark it unknown.
		UnknownRates: usageledger.UnknownRates{
			Reasoning:  p.ReasoningPerMillion == 0,
			CacheRead:  p.CacheReadPerMillion == 0,
			CacheWrite: p.CacheWritePerMillion == 0,
		},
	}
}

// eligible returns the targets of a model that can serve this request on this
// endpoint: same-dialect relay targets, plus cross-dialect targets that have
// opted in to translation and have a Translator for (clientDialect, upstreamDialect, op).
func (g *Gateway) eligible(d Dialect, op Operation, meta Meta) []*target {
	kind := g.kinds[meta.Model]
	if kind != op && !(kind == OpChat && op == OpResponses) {
		return nil
	}
	var out []*target
	for _, t := range g.models[meta.Model] {
		if len(t.operations) > 0 && !slices.Contains(t.operations, op) {
			continue
		}
		caps := t.caps
		if g.catalog != nil {
			caps, _, _, _, _ = g.catalog.ResolveTargetDetails(t)
		}
		if !meta.satisfiedBy(caps) {
			continue
		}
		upDialect := t.provider.dialect
		if upDialect.Name() == d.Name() {
			if d.UpstreamPath(op) != "" {
				out = append(out, t)
			}
			continue
		}
		if t.translate && lookupTranslator(d.Name(), upDialect.Name(), op) != nil && upDialect.UpstreamPath(op) != "" {
			out = append(out, t)
		}
	}
	return out
}

func callerOf(r *http.Request) Caller {
	if p, ok := auth.FromContext(r.Context()); ok && p.ID != "" {
		return Caller{ID: p.ID, Kind: p.Kind, Known: p.Verified}
	}
	return Caller{ID: "anonymous", Kind: "anonymous"}
}

// Handler returns the relay for one dialect and operation.
func (g *Gateway) Handler(d Dialect, op Operation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.relay(w, r, d, op) })
}

// refuse answers a request the gateway itself turns away, and logs why. The
// log line carries the stable code, never the request.
func (g *Gateway) refuse(w http.ResponseWriter, d Dialect, caller Caller, requestID, model string, status int, code, msg string) {
	g.logger.Warn("llm.refused", "request_id", requestID, "principal", caller.ID, "verified", caller.Known,
		"dialect", d.Name(), "model", model, "status", status, "code", code)
	d.WriteError(w, status, code, msg)
}

func (g *Gateway) relay(w http.ResponseWriter, r *http.Request, d Dialect, op Operation) {
	caller, requestID := callerOf(r), platform.RequestID(r)
	conversation, err := conversationOf(r, caller)
	if err != nil {
		g.refuse(w, d, caller, requestID, "", http.StatusBadRequest, "invalid_session", err.Error())
		return
	}
	caller.ConversationID = conversation
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			g.refuse(w, d, caller, requestID, "", http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		} else {
			g.refuse(w, d, caller, requestID, "", http.StatusBadRequest, "invalid_request", "request body could not be read")
		}
		return
	}
	meta, err := d.Inspect(op, body)
	if err != nil {
		g.refuse(w, d, caller, requestID, "", http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if _, ok := g.models[meta.Model]; !ok {
		g.refuse(w, d, caller, requestID, meta.Model, http.StatusNotFound, "model_not_found", "the requested model does not exist")
		return
	}
	bstat, allowed := g.checkBudget(w, r, d, caller, requestID, meta.Model)
	if !allowed {
		return
	}
	if !g.gate.hasTargetRules {
		if ok, refusal := g.gate.check(r.Context(), caller, requestID, op, meta, nil, bstat); !ok {
			if refusal.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfter))
			}
			g.refuse(w, d, caller, requestID, meta.Model, refusal.status, refusal.code, refusal.message)
			return
		}
	}
	candidates := g.eligible(d, op, meta)
	if len(candidates) == 0 {
		g.refuse(w, d, caller, requestID, meta.Model, http.StatusBadRequest, "model_not_available",
			"the model has no target that supports this request on this endpoint")
		return
	}
	targets := candidates
	if g.gate.hasTargetRules {
		targets = make([]*target, 0, len(candidates))
		var lastRefusal policyRefusal
		for _, t := range candidates {
			ok, refusal := g.gate.check(r.Context(), caller, requestID, op, meta, t, bstat)
			if ok {
				targets = append(targets, t)
			} else {
				lastRefusal = refusal
			}
		}
		if len(targets) == 0 {
			if lastRefusal.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(lastRefusal.retryAfter))
			}
			g.refuse(w, d, caller, requestID, meta.Model, lastRefusal.status, lastRefusal.code, lastRefusal.message)
			return
		}
	}

	var (
		wait    time.Duration
		tried   bool
		lastErr error
	)
	for i, t := range targets {
		// A failed attempt may consume the last request or leave unknown spend.
		// Recheck before dispatching a fallback, not only before the first attempt.
		if i > 0 {
			if _, allowed := g.checkBudget(w, r, d, caller, requestID, meta.Model); !allowed {
				return
			}
		}
		last := i == len(targets)-1
		tguard := g.guardFor(t)
		ticket, dec := tguard.Admit(guard.Key{Resource: t.provider.id, Model: t.model})
		if !dec.Allowed {
			wait = shortest(wait, dec.RetryAfter)
			g.logger.Warn("target refused", "request_id", requestID, "provider", t.provider.id, "model", t.model, "reason", dec.Reason.String())
			continue
		}
		tried = true
		if t.provider.opencodeSession && caller.ConversationID == "" {
			ticket.Release()
			g.refuse(w, d, caller, requestID, meta.Model, http.StatusBadRequest, "session_required", "this provider requires X-Station-Session: a stable opaque conversation ID")
			return
		}
		start := time.Now()

		upDialect := t.provider.dialect
		var (
			tr       Translator
			out      []byte
			injected bool
		)
		if upDialect.Name() == d.Name() {
			out, injected = d.Rewrite(op, body, meta, t.model)
		} else {
			tr = lookupTranslator(d.Name(), upDialect.Name(), op)
			var terr error
			out, terr = tr.TranslateRequest(op, body, t.model)
			if terr != nil {
				ticket.Release()
				g.refuse(w, d, caller, requestID, meta.Model, http.StatusBadRequest, "invalid_request", terr.Error())
				return
			}
		}

		reqCtx := r.Context()
		var (
			cancelAttempt context.CancelFunc
			headerTimer   *time.Timer
			headerExpired atomic.Bool
		)
		if t.headerTimeout > 0 {
			reqCtx, cancelAttempt = context.WithCancel(r.Context())
		}

		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.provider.baseURL+upDialect.UpstreamPath(op), bytes.NewReader(out))
		if err != nil {
			if cancelAttempt != nil {
				cancelAttempt()
			}
			ticket.Release()
			g.refuse(w, d, caller, requestID, meta.Model, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		upDialect.Authorize(req, r, t.provider.apiKey)
		for k, v := range t.provider.headers {
			req.Header.Set(k, v)
		}
		if t.provider.opencodeSession {
			req.Header.Set("X-Opencode-Session", caller.ConversationID)
		}
		// The meter must read plain bytes.
		req.Header.Set("Accept-Encoding", "identity")

		// Freeze this attempt's rates, including an absent price. Catalog refreshes
		// during the request must not rewrite the cost of work already dispatched.
		attempt := *t
		attempt.price = g.catalog.ResolvePrice(t)
		if attempt.price != nil {
			price := *attempt.price
			attempt.price = &price
		}
		t = &attempt
		reservation, bstat, reserveErr := g.budgets.Reserve(reqCtx, caller, requestID, meta.Model, op, d.Name(), t, meta.Stream)
		if reserveErr != nil || bstat.State != "ok" {
			if cancelAttempt != nil {
				cancelAttempt()
			}
			ticket.Release()
			refusal := budgetRefusal(bstat)
			if reserveErr != nil {
				refusal = policyRefusal{status: http.StatusServiceUnavailable, code: "budget_unavailable", message: "usage budget could not be reserved"}
			}
			if refusal.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfter))
			}
			g.refuse(w, d, caller, requestID, meta.Model, refusal.status, refusal.code, refusal.message)
			return
		}
		t.reservation = reservation
		if t.headerTimeout > 0 {
			headerTimer = time.AfterFunc(t.headerTimeout, func() {
				headerExpired.Store(true)
				cancelAttempt()
			})
		}
		resp, err := t.provider.client.Do(req)
		if headerTimer != nil {
			headerTimer.Stop()
		}
		if err != nil {
			if cancelAttempt != nil {
				cancelAttempt()
			}
			if headerExpired.Load() && r.Context().Err() == nil {
				err = &guard.Error{Class: guard.ClassTransient, Err: fmt.Errorf("target header timeout (%s): %w", t.headerTimeout, err)}
			}
			class := ticket.Done(err)
			if r.Context().Err() != nil {
				g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
					Outcome: OutcomeCanceled, Streamed: meta.Stream, Latency: time.Since(start)})
				return // the client left; the dispatched attempt still counts
			}
			g.logger.Warn("target failed", "request_id", requestID, "provider", t.provider.id, "model", t.model, "class", class.String(), "err", err)
			g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
				Outcome: OutcomeFailedOver, Streamed: meta.Stream, Latency: time.Since(start)})
			lastErr = err
			continue
		}

		if resp.StatusCode >= 400 {
			herr := guard.HTTPError(resp.StatusCode, resp.Header, fmt.Errorf("upstream status %d", resp.StatusCode))
			class := ticket.Done(herr)
			if !last && failoverClass(class) {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if cancelAttempt != nil {
					cancelAttempt()
				}
				g.logger.Warn("target failed over", "request_id", requestID, "provider", t.provider.id, "model", t.model, "status", resp.StatusCode, "class", class.String())
				g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
					Status: resp.StatusCode, Outcome: OutcomeFailedOver, Streamed: meta.Stream, Latency: time.Since(start)})
				lastErr = herr
				continue
			}
			if tr != nil {
				errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, int64(g.capturer.MaxBytes())))
				resp.Body.Close()
				if cancelAttempt != nil {
					cancelAttempt()
				}
				w.Header().Set("X-Request-Id", requestID)
				d.WriteError(w, resp.StatusCode, "upstream_error", "the upstream provider returned an error")
				detail := g.capturer.BuildDetail(body, errBytes, nil, nil)
				g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
					Detail: detail, Status: resp.StatusCode, Outcome: OutcomeUpstreamErr, Streamed: meta.Stream, Latency: time.Since(start)})
				return
			}
			// Transparent: the upstream's own error reaches the client untouched.
			g.pass(w, r, d, op, resp, t, caller, requestID, meta, body, injected, start)
			if cancelAttempt != nil {
				cancelAttempt()
			}
			return
		}
		ticket.Done(nil)
		if tr != nil {
			g.passTranslated(w, r, d, op, tr, resp, t, caller, requestID, meta, body, start)
		} else {
			g.pass(w, r, d, op, resp, t, caller, requestID, meta, body, injected, start)
		}
		if cancelAttempt != nil {
			cancelAttempt()
		}
		return
	}

	if !tried {
		if secs := int(math.Ceil(wait.Seconds())); secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		g.refuse(w, d, caller, requestID, meta.Model, http.StatusServiceUnavailable, "model_unavailable", "the model is temporarily unavailable")
		return
	}
	g.logger.Error("all targets failed", "request_id", requestID, "model", meta.Model, "last", lastErr)
	g.refuse(w, d, caller, requestID, meta.Model, http.StatusBadGateway, "upstream_error", "the upstream provider failed")
}

// failoverClass is the set of failures another target might not share. A
// request error (4xx) is the caller's and is never retried elsewhere.
func failoverClass(c guard.Class) bool {
	switch c {
	case guard.ClassQuota, guard.ClassAuth, guard.ClassTransient, guard.ClassConnection:
		return true
	}
	return false
}

func shortest(cur, next time.Duration) time.Duration {
	if cur == 0 || (next > 0 && next < cur) {
		return next
	}
	return cur
}

// pass streams the upstream response to the client while a meter reads usage.
// Headers go out as soon as they are known; every forwarded SSE line is
// flushed, so first-byte latency is the upstream's.
func (g *Gateway) pass(w http.ResponseWriter, r *http.Request, d Dialect, op Operation, resp *http.Response, t *target,
	caller Caller, requestID string, meta Meta, reqBody []byte, injected bool, start time.Time) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if droppedResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		w.Header()[k] = append([]string(nil), vs...)
	}
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)

	sse := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	meter := d.NewMeter(op, meta.Stream, injected)
	outcome := OutcomeOK
	if resp.StatusCode >= 400 {
		outcome = OutcomeUpstreamErr
	}

	var (
		copyErr   error
		sseEvents []json.RawMessage
		respBytes []byte
	)
	maxCap := 0
	if g.capturer.Enabled() {
		maxCap = g.capturer.MaxBytes()
	}
	if sse {
		sseEvents, copyErr = relaySSE(w, rc, resp.Body, meter, maxCap)
	} else {
		var buf bytes.Buffer
		over := false
		dst := writerFunc(func(p []byte) (int, error) {
			if !over {
				if buf.Len()+len(p) > maxMeteredBody {
					over = true
					buf.Reset()
				} else {
					buf.Write(p)
				}
			}
			n, err := w.Write(p)
			_ = rc.Flush()
			return n, err
		})
		_, copyErr = io.Copy(dst, resp.Body)
		if copyErr == nil && !over {
			respBytes = buf.Bytes()
			if resp.StatusCode < 400 {
				meter.Body(respBytes)
			}
		}
	}
	if copyErr != nil {
		outcome = OutcomeInterrupted
		if r.Context().Err() != nil {
			outcome = OutcomeCanceled
		}
	}
	var (
		usage       *Usage
		meterDetail json.RawMessage
	)
	if resp.StatusCode < 400 {
		usage = meter.Usage()
		if dm, ok := meter.(interface{ Detail() json.RawMessage }); ok {
			meterDetail = dm.Detail()
		}
	}
	detail := g.capturer.BuildDetail(reqBody, respBytes, sseEvents, meterDetail)
	g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
		Usage: usage, Detail: detail, Status: resp.StatusCode, Outcome: outcome, Streamed: sse, Latency: time.Since(start)})
}

// passTranslated adapts an upstream response (or SSE stream) from a
// cross-dialect target into the client's dialect.
func (g *Gateway) passTranslated(w http.ResponseWriter, r *http.Request, d Dialect, op Operation, tr Translator,
	resp *http.Response, t *target, caller Caller, requestID string, meta Meta, reqBody []byte, start time.Time) {
	defer resp.Body.Close()
	sse := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	w.Header().Set("X-Request-Id", requestID)
	rc := http.NewResponseController(w)

	maxCap := 0
	if g.capturer.Enabled() {
		maxCap = g.capturer.MaxBytes()
	}

	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		usage, sseEvents, err := tr.TranslateSSE(w, rc, resp.Body, meta.Model, maxCap)
		outcome := OutcomeOK
		if err != nil {
			outcome = OutcomeInterrupted
			if r.Context().Err() != nil {
				outcome = OutcomeCanceled
			}
		}
		detail := g.capturer.BuildDetail(reqBody, nil, sseEvents, nil)
		g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
			Usage: usage, Detail: detail, Status: resp.StatusCode, Outcome: outcome, Streamed: true, Latency: time.Since(start)})
		return
	}

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, maxMeteredBody))
	if err != nil {
		d.WriteError(w, http.StatusBadGateway, "upstream_error", "the upstream provider failed")
		g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
			Status: http.StatusBadGateway, Outcome: OutcomeInterrupted, Streamed: false, Latency: time.Since(start)})
		return
	}
	outBytes, usage, err := tr.TranslateResponse(op, rawResp, meta.Model)
	if err != nil {
		d.WriteError(w, http.StatusBadGateway, "upstream_error", "the upstream provider failed")
		g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
			Status: http.StatusBadGateway, Outcome: OutcomeUpstreamErr, Streamed: false, Latency: time.Since(start)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(outBytes)
	_ = rc.Flush()
	detail := g.capturer.BuildDetail(reqBody, outBytes, nil, nil)
	g.record(Record{Caller: caller, RequestID: requestID, Alias: meta.Model, Operation: op, Dialect: d.Name(), Target: t,
		Usage: usage, Detail: detail, Status: resp.StatusCode, Outcome: OutcomeOK, Streamed: false, Latency: time.Since(start)})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// relaySSE forwards an event stream line by line, letting the meter read each
// line and withhold only what it injected itself. When maxCapture > 0, JSON
// data events are also collected for sanitized payload capture.
func relaySSE(w io.Writer, rc *http.ResponseController, body io.Reader, meter Meter, maxCapture int) ([]json.RawMessage, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var (
		line     []byte
		events   []json.RawMessage
		capBytes int
	)
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxSSELine {
				return events, errors.New("sse line too long")
			}
			continue
		}
		if len(line) > 0 {
			if meter.Line(line) {
				if maxCapture > 0 {
					if data, ok := sseData(line); ok && string(data) != "[DONE]" && json.Valid(data) && capBytes+len(data) <= maxCapture {
						events = append(events, json.RawMessage(append([]byte(nil), data...)))
						capBytes += len(data)
					}
				}
				if _, werr := w.Write(line); werr != nil {
					return events, werr
				}
				if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
					return events, ferr
				}
			}
			line = line[:0]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return events, err
		}
	}
}

// modelsFor lists the models that can be served on d (either natively or via
// cross-protocol translation), along with live reachability, route tags, and
// resolved catalog metadata, capabilities, limits, modalities, and pricing.
func (g *Gateway) modelsFor(d Dialect) []ModelInfo {
	var out []ModelInfo
	for _, n := range g.names {
		op := g.kinds[n]
		if op == OpDecide {
			continue
		}
		var (
			tinfos    []TargetInfo
			tags      []string
			reachable bool
			aggCaps   *TargetCapabilities
		)
		for _, t := range g.models[n] {
			upDialect := t.provider.dialect
			if upDialect.Name() != d.Name() && (!t.translate || lookupTranslator(d.Name(), upDialect.Name(), op) == nil) {
				continue
			}
			key := guard.Key{Resource: t.provider.id, Model: t.model}
			tguard := g.guardFor(t)
			ok := tguard.Check(key).Allowed
			if ok {
				reachable = true
			}
			for _, tag := range t.tags {
				if !slices.Contains(tags, tag) {
					tags = append(tags, tag)
				}
			}
			_, viewCaps, limits, modalities, meta := g.catalog.ResolveTargetDetails(t)
			price := g.catalog.ResolvePrice(t)
			_, catalogProvider, catalogMatched := g.catalog.LookupModel(t.provider.catalogProvider, t.provider.id, t.model)
			vc := viewCaps
			if aggCaps == nil {
				c := vc
				aggCaps = &c
			} else {
				aggCaps.Tools = aggCaps.Tools || vc.Tools
				aggCaps.Vision = aggCaps.Vision || vc.Vision
				aggCaps.Streaming = aggCaps.Streaming || vc.Streaming
				aggCaps.Reasoning = aggCaps.Reasoning || vc.Reasoning
				aggCaps.Attachment = aggCaps.Attachment || vc.Attachment
				aggCaps.Temperature = aggCaps.Temperature || vc.Temperature
			}
			tinfos = append(tinfos, TargetInfo{
				Operations:      t.operations,
				Provider:        t.provider.id,
				Model:           t.model,
				Dialect:         upDialect.Name(),
				CatalogProvider: catalogProvider,
				CatalogMatched:  catalogMatched,
				Tags:            t.tags,
				BestEffort:      t.bestEffort,
				Reachable:       ok,
				State:           tguard.State(key).String(),
				Capabilities:    &vc,
				Price:           price,
				Limits:          limits,
				Modalities:      modalities,
				Metadata:        meta,
			})
		}
		if len(tinfos) > 0 {
			primary := tinfos[0]
			displayName := ""
			if primary.Metadata != nil && primary.Metadata.Name != "" {
				displayName = primary.Metadata.Name
			}
			out = append(out, ModelInfo{
				Name:         n,
				DisplayName:  displayName,
				Kind:         op,
				Reachable:    reachable,
				Tags:         tags,
				Capabilities: aggCaps,
				Price:        primary.Price,
				Limits:       primary.Limits,
				Modalities:   primary.Modalities,
				Metadata:     primary.Metadata,
				Targets:      tinfos,
			})
		}
	}
	return out
}

// ModelsHandler answers GET /v1/models in the shape the caller expects: an
// Anthropic client announces itself with the anthropic-version header.
func (g *Gateway) ModelsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := Dialect(openaiDialect{})
		if r.Header.Get("Anthropic-Version") != "" {
			d = anthropicDialect{}
		}
		d.ListModels(w, g.modelsFor(d))
	})
}

// CatalogHandler answers GET /v1/catalog with the centralized models.dev
// catalog (providers, models, capabilities, pricing, context/output limits,
// modalities) and the gateway's resolved model configurations.
// Optional query filters: provider, model, family.
func (g *Gateway) CatalogHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		provFilter := q.Get("provider")
		modelFilter := q.Get("model")
		familyFilter := q.Get("family")

		var (
			fetchedAt time.Time
			ttlSec    int64
			providers []CatalogProviderSummary
			models    []CatalogModelEntry
		)
		if g.catalog != nil {
			fetchedAt = g.catalog.LastFetchedAt()
			ttlSec = int64(g.catalog.TTL().Seconds())
			providers = g.catalog.ListProviders(provFilter)
			models = g.catalog.ListModels(provFilter, modelFilter, familyFilter)
		}
		out := map[string]any{
			"schema_version": 1,
			"status":         g.catalog.Status(),
			"ttl_seconds":    ttlSec,
			"providers":      providers,
			"models":         models,
		}
		if !fetchedAt.IsZero() {
			out["fetched_at"] = fetchedAt.UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// CatalogRefreshHandler answers POST /v1/catalog/refresh by triggering an
// immediate refresh from the models.dev source and updating its cache.
// The operator price file is reloaded separately.
func (g *Gateway) CatalogRefreshHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.catalog == nil {
			openaiDialect{}.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "the model catalog is not configured")
			return
		}
		if err := g.catalog.Refresh(r.Context()); err != nil {
			g.logger.Warn("catalog.refresh_failed", "request_id", platform.RequestID(r), "err", err)
			openaiDialect{}.WriteError(w, http.StatusBadGateway, "catalog_refresh_failed", "the model catalog could not be refreshed")
			return
		}
		fetchedAt := g.catalog.LastFetchedAt()
		providers := g.catalog.ListProviders("")
		models := g.catalog.ListModels("", "", "")
		out := map[string]any{
			"schema_version": 1,
			"status":         g.catalog.Status(),
			"ttl_seconds":    int64(g.catalog.TTL().Seconds()),
			"provider_count": len(providers),
			"model_count":    len(models),
		}
		if !fetchedAt.IsZero() {
			out["fetched_at"] = fetchedAt.UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// CatalogPricesReloadHandler reloads only the configured local price table.
func (g *Gateway) CatalogPricesReloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.catalog == nil {
			openaiDialect{}.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "the model catalog is not configured")
			return
		}
		if err := g.catalog.ReloadPrices(r.Context()); err != nil {
			status := http.StatusUnprocessableEntity
			if errors.Is(err, errPriceFileNotConfigured) {
				status = http.StatusConflict
			}
			// Health carries a safe error class; never return a path or file contents.
			writeJSON(w, status, map[string]any{"schema_version": 1, "error": map[string]string{"code": "price_file_reload_failed", "message": "the price file could not be reloaded"}, "status": g.catalog.Status()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"schema_version": 1, "status": g.catalog.Status()})
	})
}

// UsageHandler answers GET /v1/usage. Filters: limit (1..1000), principal,
// since (RFC 3339). It reads the ledger, so on the durable store it survives
// restarts.
func (g *Gateway) UsageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bad := func(msg string) {
			openaiDialect{}.WriteError(w, http.StatusBadRequest, "invalid_request", msg)
		}
		q := Query{Limit: 100, Principal: r.URL.Query().Get("principal")}
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				bad("limit must be between 1 and 1000")
				return
			}
			q.Limit = n
		}
		if v := r.URL.Query().Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				bad("since must be an RFC 3339 time")
				return
			}
			q.Since = t
		}
		entries, totals, stats, err := g.ledger.Snapshot(r.Context(), q)
		if err != nil {
			g.logger.Error("ledger.query_failed", "request_id", platform.RequestID(r), "err", err)
			openaiDialect{}.WriteError(w, http.StatusServiceUnavailable, "ledger_unavailable", "the usage ledger could not be read")
			return
		}
		out := map[string]any{"entries": entries, "totals": totals}
		if stats != nil {
			out["buffer"] = stats
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// checkBudget enforces configured budgets independently of policy evaluation.
func (g *Gateway) checkBudget(w http.ResponseWriter, r *http.Request, d Dialect, caller Caller, requestID, model string) (BudgetStatus, bool) {
	status, err := g.budgets.CheckStored(r.Context(), caller, model)
	if err != nil {
		g.refuse(w, d, caller, requestID, model, http.StatusServiceUnavailable, "budget_unavailable", "usage budget could not be evaluated")
		return status, false
	}
	if status.State != "ok" {
		refusal := budgetRefusal(status)
		if refusal.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfter))
		}
		g.refuse(w, d, caller, requestID, model, refusal.status, refusal.code, refusal.message)
		return status, false
	}
	return status, true
}
