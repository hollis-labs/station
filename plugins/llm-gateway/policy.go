package llmgateway

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"

	"github.com/hollis-labs/libs/security/policy"
)

// Caller is the verified identity behind a request. It is built only from the
// principal Station's auth guard stamped on the request context. Nothing the
// client puts in a header or body (a caller_id, a session label) is read, so
// an attribution can never be self-asserted.
type Caller struct {
	ID    string
	Kind  string
	Known bool // false when no principal was stamped (auth off or permissive)
}

func (c Caller) principal() policy.Principal {
	id, kind := c.ID, c.Kind
	if !c.Known {
		id, kind = "anonymous", "anonymous"
	}
	if kind == "" {
		kind = "service"
	}
	return policy.Principal{URN: "urn:station:principal:" + id, Kind: kind}
}

// gate is the llm.invoke enforcement point. Every relayed call asks it first.
type gate struct {
	eval           policy.Evaluator
	opts           policy.Options
	hasTargetRules bool
	logger         *slog.Logger
}

func newGate(cfg PolicyConfig, logger *slog.Logger) *gate {
	mode := policy.Shadow
	if cfg.Mode == "enforce" {
		mode = policy.Enforce
	}
	def := policy.Effect("allow")
	if cfg.DefaultEffect != "" {
		def = policy.Effect(cfg.DefaultEffect)
	}
	hasTargetRules := false
	for _, r := range cfg.Rules {
		if r.Provider != "" || r.Tag != "" {
			hasTargetRules = true
			break
		}
	}
	return &gate{
		eval:           policy.Evaluator{PDP: rulePDP{rules: cfg.Rules, def: def}, Audit: slogAudit{logger}},
		opts:           policy.Options{Mode: mode, FailMode: policy.FailClosed},
		hasTargetRules: hasTargetRules,
		logger:         logger,
	}
}

type policyRefusal struct {
	status     int
	code       string
	message    string
	retryAfter int
}

func (r policyRefusal) write(w http.ResponseWriter, d Dialect) {
	if r.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(r.retryAfter))
	}
	d.WriteError(w, r.status, r.code, r.message)
}

// check evaluates llm.invoke for the request (and optional target t) with
// budget_state facts from the quota evaluator. It returns (true, zero) when
// admitted, or (false, refusal) when blocked.
func (g *gate) check(ctx context.Context, caller Caller, requestID string, op Operation, meta Meta, t *target, bstat BudgetStatus) (bool, policyRefusal) {
	if bstat.State == "" {
		bstat.State = "ok"
	}
	facts := map[string]any{
		"principal_id":      caller.ID,
		"operation":         string(op),
		"stream":            meta.Stream,
		"tools":             meta.Needs.Tools,
		"vision":            meta.Needs.Vision,
		"budget_state":      bstat.State,
		"exhausted_budgets": bstat.Exhausted,
	}
	if bstat.RemainingUSD != nil {
		facts["budget_remaining_usd"] = *bstat.RemainingUSD
	}
	if t != nil {
		facts["provider"] = t.provider.id
		facts["target_model"] = t.model
		facts["tags"] = t.tags
	}

	res, err := g.eval.Evaluate(ctx, policy.Request{
		RequestID: requestID,
		Principal: caller.principal(),
		Action:    "llm.invoke",
		Resource:  policy.Resource{Type: "llm-model", ID: meta.Model},
		Scope:     "llm-gateway",
		Context:   facts,
	}, g.opts)
	if err != nil {
		g.logger.Error("policy evaluation failed", "request_id", requestID, "failure", string(res.Failure))
	}
	if res.WouldBlock && res.Admission == policy.Allow {
		g.logger.Warn("policy would block",
			"request_id", requestID,
			"model", meta.Model,
			"effect", string(res.Decision.Effect),
			"mode", string(g.opts.Mode),
			"budget_state", bstat.State,
			"exhausted_budgets", bstat.Exhausted,
		)
	}
	switch res.Admission {
	case policy.Allow:
		return true, policyRefusal{}
	case policy.Ask:
		return false, policyRefusal{
			status:  http.StatusForbidden,
			code:    "approval_required",
			message: "this request requires approval",
		}
	case policy.Deny:
		if res.Failure != "" {
			return false, policyRefusal{
				status:  http.StatusServiceUnavailable,
				code:    "policy_unavailable",
				message: "policy could not be evaluated",
			}
		}
		if isBudgetDecision(res.Decision) {
			secs := int(math.Ceil(bstat.RetryAfter.Seconds()))
			return false, policyRefusal{
				status:     http.StatusTooManyRequests,
				code:       "budget_exceeded",
				message:    "usage budget exceeded",
				retryAfter: secs,
			}
		}
		return false, policyRefusal{
			status:  http.StatusForbidden,
			code:    "policy_denied",
			message: "policy does not allow this request",
		}
	default:
		return false, policyRefusal{
			status:  http.StatusServiceUnavailable,
			code:    "policy_unavailable",
			message: "policy could not be evaluated",
		}
	}
}

func isBudgetDecision(d policy.Decision) bool {
	for _, m := range d.MatchedPolicies {
		if m.Source == "llm-gateway/budgets" {
			return true
		}
	}
	return false
}

// rulePDP is a small first-match decision point over the config rules. The
// policy library deliberately ships no engine; this is the gateway's own.
type rulePDP struct {
	rules []RuleConfig
	def   policy.Effect
}

func (p rulePDP) Evaluate(_ context.Context, req policy.Request) (policy.Decision, error) {
	id, _ := req.Context["principal_id"].(string)
	providerID, _ := req.Context["provider"].(string)
	tags, _ := req.Context["tags"].([]string)
	bstate, _ := req.Context["budget_state"].(string)

	for _, r := range p.rules {
		if !glob(r.Principal, id) || !glob(r.Model, req.Resource.ID) {
			continue
		}
		if r.Provider != "" && !glob(r.Provider, providerID) {
			continue
		}
		if r.Tag != "" && !slices.Contains(tags, r.Tag) {
			continue
		}
		if r.Operation != "" {
			if o, _ := req.Context["operation"].(string); o != r.Operation {
				continue
			}
		}
		if r.Needs != "" {
			key := r.Needs
			if key == "streaming" {
				key = "stream"
			}
			if on, _ := req.Context[key].(bool); !on {
				continue
			}
		}
		if r.BudgetState != "" && r.BudgetState != bstate {
			continue
		}
		src := "llm-gateway/config"
		if r.BudgetState == "exhausted" && r.Effect == "deny" {
			src = "llm-gateway/budgets"
		}
		return policy.Decision{
			Effect:          policy.Effect(r.Effect),
			Reason:          r.Reason,
			MatchedPolicies: []policy.PolicyMatch{{ID: r.ID, Source: src, Reason: r.Reason}},
		}, nil
	}
	if bstate == "exhausted" {
		exhausted, _ := req.Context["exhausted_budgets"].([]string)
		matches := make([]policy.PolicyMatch, 0, max(len(exhausted), 1))
		for _, bid := range exhausted {
			matches = append(matches, policy.PolicyMatch{
				ID:     "budget:" + bid,
				Source: "llm-gateway/budgets",
				Reason: "budget exhausted",
			})
		}
		if len(matches) == 0 {
			matches = append(matches, policy.PolicyMatch{
				ID:     "budget",
				Source: "llm-gateway/budgets",
				Reason: "budget exhausted",
			})
		}
		return policy.Decision{
			Effect:          policy.Deny,
			Reason:          "budget exhausted",
			MatchedPolicies: matches,
		}, nil
	}
	return policy.Decision{Effect: p.def, Reason: "default"}, nil
}

func glob(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

// slogAudit records each evaluation. The policy library guarantees the event
// holds no request context, so nothing sensitive reaches the log.
type slogAudit struct{ l *slog.Logger }

func (a slogAudit) Record(_ context.Context, ev policy.AuditEvent) error {
	var matched []string
	for _, m := range ev.Decision.MatchedPolicies {
		matched = append(matched, m.ID)
	}
	a.l.Info("policy", "request_id", ev.RequestID, "principal", ev.Principal.URN, "action", ev.Action,
		"resource", ev.Resource.ID, "mode", string(ev.Mode), "effect", string(ev.Decision.Effect),
		"admission", string(ev.ProposedAdmission), "would_block", ev.WouldBlock, "matched", matched,
		"failure", string(ev.Failure))
	return nil
}
