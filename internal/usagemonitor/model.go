// Package usagemonitor owns subscription allowance observations and usage history.
// Provider snapshots are gauges; only individual usage events are additive.
package usagemonitor

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/hollis-labs/libs/llm/core/usageledger"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

type Account struct {
	ID       string `json:"id" yaml:"id"`
	Provider string `json:"provider" yaml:"provider"`
	Kind     string `json:"kind" yaml:"kind"` // subscription, api, local
	Label    string `json:"label" yaml:"label"`
	Plan     string `json:"plan" yaml:"plan"`
	Enabled  bool   `json:"enabled" yaml:"enabled"`
	Revision int64  `json:"revision" yaml:"-"`
}

func (a Account) validate() error {
	if !identifier.MatchString(a.ID) || !identifier.MatchString(a.Provider) || len(a.Label) > 256 || len(a.Plan) > 128 {
		return errors.New("invalid account identity or label")
	}
	switch a.Kind {
	case "subscription", "api", "local":
		return nil
	}
	return errors.New("account kind must be subscription, api or local")
}

// Window retains provider-native units. Nil means unknown, including reset time.
// Model can identify a model family. Windows may overlap and must not be summed.
type Window struct {
	ID        string     `json:"id"`
	Model     string     `json:"model,omitempty"`
	Unit      string     `json:"unit"`
	Used      *float64   `json:"used"`
	Limit     *float64   `json:"limit"`
	Remaining *float64   `json:"remaining"`
	ResetsAt  *time.Time `json:"resets_at"`
}

type Observation struct {
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
	Status     string    `json:"status"` // ok, unknown, error
	ErrorCode  string    `json:"error_code,omitempty"`
	Windows    []Window  `json:"windows"`
}

// Event uses the shared disjoint-token/provenance contract. Cost is explicitly
// typed: a subscription equivalent must never be reported as a provider bill.
type Event struct {
	ID          string          `json:"id"`
	Row         usageledger.Row `json:"row"`
	CostUSD     *float64        `json:"cost_usd"`
	CostPartial bool            `json:"cost_partial"`
	Outcome     string          `json:"outcome"`
}

type Batch struct {
	Observations []Observation `json:"observations"`
	Events       []Event       `json:"events"`
}

func finiteNonnegative(v *float64) bool {
	return v == nil || (!math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0)
}
func validTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 2000 && t.Before(time.Now().Add(5*time.Minute))
}

func (o Observation) validate() error {
	if !identifier.MatchString(o.ID) || !validTime(o.ObservedAt) || len(o.Windows) > 128 {
		return errors.New("invalid observation identity, timestamp or window count")
	}
	switch o.Status {
	case "ok":
		if len(o.Windows) == 0 || o.ErrorCode != "" {
			return errors.New("ok observation requires windows and no error")
		}
	case "unknown", "error":
		if len(o.Windows) != 0 {
			return errors.New("failed observations cannot contain quota measurements")
		}
	default:
		return errors.New("invalid observation status")
	}
	if o.ErrorCode != "" && !identifier.MatchString(o.ErrorCode) {
		return errors.New("invalid error code")
	}
	seen := map[string]bool{}
	for _, w := range o.Windows {
		if !identifier.MatchString(w.ID) || seen[w.ID] || len(w.Model) > 256 {
			return errors.New("invalid or duplicate quota window")
		}
		seen[w.ID] = true
		switch w.Unit {
		case "percent", "tokens", "requests", "credits", "usd":
		default:
			return errors.New("invalid window unit")
		}
		if !finiteNonnegative(w.Used) || !finiteNonnegative(w.Limit) || !finiteNonnegative(w.Remaining) {
			return errors.New("invalid quota quantity")
		}
		if w.Unit == "percent" && ((w.Used != nil && *w.Used > 100) || (w.Remaining != nil && *w.Remaining > 100) || (w.Limit != nil && *w.Limit != 100)) {
			return errors.New("invalid percentage")
		}
		if w.ResetsAt != nil && (w.ResetsAt.IsZero() || w.ResetsAt.Year() < 2000 || w.ResetsAt.Year() > 2200) {
			return errors.New("invalid reset timestamp")
		}
	}
	return nil
}

func (b Batch) validate(a Account) error {
	if len(b.Events)+len(b.Observations) == 0 || len(b.Events)+len(b.Observations) > 500 {
		return errors.New("batch must contain 1 to 500 records")
	}
	for _, o := range b.Observations {
		if err := o.validate(); err != nil {
			return err
		}
	}
	for _, e := range b.Events {
		if !identifier.MatchString(e.ID) || !validTime(e.Row.RecordedAt) || e.Row.Provider != a.Provider || len(e.Row.Model) > 256 || len(e.Row.SessionID) > 256 || len(e.Row.MessageID) > 256 || !identifier.MatchString(e.Outcome) {
			return errors.New("invalid usage event identity, provider or timestamp")
		}
		if err := e.Row.Validate(); err != nil {
			return fmt.Errorf("invalid usage: %w", err)
		}
		if !finiteNonnegative(e.CostUSD) || (e.CostUSD != nil && e.Row.CostKind == usageledger.CostKindUnspecified) {
			return errors.New("cost requires a valid quantity and explicit cost kind")
		}
		if a.Kind == "subscription" && (e.Row.CostKind == usageledger.CostKindAPIBilled || e.Row.CostKind == usageledger.CostKindAPIEstimated) {
			return errors.New("subscription events must use subscription_equivalent cost kind")
		}
	}
	return nil
}

type Record[T any] struct {
	Sequence    int64     `json:"sequence"`
	AccountID   string    `json:"account_id"`
	Source      string    `json:"source"`
	Actor       string    `json:"actor"`
	CollectedAt time.Time `json:"collected_at"`
	Data        T         `json:"data"`
}

type Page[T any] struct {
	Items     []T   `json:"items"`
	NextAfter int64 `json:"next_after"`
	HasMore   bool  `json:"has_more"`
}

type AccountView struct {
	Account
	Latest      *Record[Observation] `json:"latest_attempt"`
	LastSuccess *Record[Observation] `json:"last_success"`
	Stale       bool                 `json:"stale"`
}
