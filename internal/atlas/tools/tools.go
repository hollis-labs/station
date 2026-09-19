// Package tools is the Atlas server's MCP surface. Every tool is scoped to the
// Atlas namespace: none takes a namespace argument, and the class of a record
// comes from its key. The descriptions are the process documentation agents
// read, so they say what a tool is for and what it will not do, not just what
// it returns.
//
// This is the read slice. Nothing here writes to Tesseract, and every tool
// says so through its annotations.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hollis-labs/go-mcp/budget"
	gmcpserver "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

// Deps are what the tools read through.
type Deps struct {
	Client *tesseract.Client
	Store  *corpus.Store
	// Now is the clock, replaceable in tests; nil means time.Now.
	Now func() time.Time
}

type handler func(ctx context.Context, args map[string]any) (any, error)

type definition struct {
	name        string
	description string
	props       []gmcpserver.Prop
	run         handler
}

type service struct{ Deps }

// Register adds the read tools to srv.
func Register(srv *gmcpserver.Server, d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &service{d}
	for _, def := range s.definitions() {
		srv.RegisterTool(gmcpserver.Tool{
			Name:        def.name,
			Description: def.description,
			InputSchema: gmcpserver.InputSchema(def.props...),

			// Read-only over a closed, local system: nothing here changes
			// Tesseract, and the same call twice reads the same thing.
			ReadOnlyHint:    true,
			DestructiveHint: false,
			IdempotentHint:  true,
			OpenWorldHint:   false,

			Handler: guarded(def),
		})
	}
}

// Names lists the registered tool names in registration order.
func Names() []string {
	defs := (&service{}).definitions()
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.name
	}
	return names
}

func (s *service) definitions() []definition {
	return []definition{
		s.guideTool(), s.getTool(), s.searchTool(), s.listTool(),
		s.backlinksTool(), s.reviewQueueTool(), s.auditTool(), s.tagsTool(),
	}
}

// guarded rejects what the SDK's low-level tool registration does not: an
// argument the tool does not take (a misspelled filter is otherwise silently
// ignored, and the caller reads an unfiltered answer as a filtered one) and a
// missing required argument.
func guarded(def definition) gmcpserver.ToolHandler {
	allowed := map[string]bool{}
	for _, p := range def.props {
		allowed[p.Name] = true
	}
	return func(ctx context.Context, args map[string]any) (any, error) {
		for k := range args {
			if !allowed[k] {
				names := make([]string, 0, len(allowed))
				for n := range allowed {
					names = append(names, n)
				}
				sort.Strings(names)
				return nil, invalid(k, fmt.Sprintf("%s takes no argument %q; it accepts: %s", def.name, k, strings.Join(names, ", ")))
			}
		}
		for _, p := range def.props {
			if p.Required && argString(args, p.Name) == "" {
				return nil, invalid(p.Name, fmt.Sprintf("%s requires %q", def.name, p.Name))
			}
		}
		return def.run(ctx, args)
	}
}

// Errors carry a code, and point at atlas_guide, so a caller that got it wrong
// is told what would have been right.

func invalid(field, msg string) error {
	return budget.NewToolError("invalid_argument", msg).WithField(field).WithHelpTool("atlas_guide")
}

func notFound(msg, next string) error {
	e := budget.NewToolError("not_found", msg).WithHelpTool("atlas_guide")
	if next != "" {
		e = e.WithNextStep(next)
	}
	return e
}

func upstream(err error) error {
	if errors.Is(err, tesseract.ErrUnavailable) {
		return budget.NewToolError("tesseract_unavailable", err.Error()).
			WithRetryable(true).
			WithNextStep("Check that Tesseract is running and that TESSERACT_URL points at it, then retry.")
	}
	return budget.NewToolError("tesseract_error", err.Error())
}

// Argument readers. MCP delivers numbers as float64 and arrays as []any.

func argString(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return strings.TrimSpace(s)
}

func argInt(a map[string]any, k string, def, lo, hi int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	return max(lo, min(hi, n))
}

func argBool(a map[string]any, k string) bool {
	b, _ := a[k].(bool)
	return b
}

func argStrings(a map[string]any, k string) []string {
	list, _ := a[k].([]any)
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// classArg reads a class argument: a class key, display name or prefix token,
// or "document" where allowed. It returns the class key, DocumentClass, or ""
// when the argument is absent and not required.
func classArg(a map[string]any, name string, allowDocument, required bool) (string, error) {
	v := argString(a, name)
	if v == "" {
		if required {
			return "", invalid(name, fmt.Sprintf("%q is required; use one of %s", name, classChoices(allowDocument)))
		}
		return "", nil
	}
	if allowDocument && strings.EqualFold(v, corpus.DocumentClass) {
		return corpus.DocumentClass, nil
	}
	c, ok := contract.ClassByName(v)
	if !ok {
		return "", invalid(name, fmt.Sprintf("unknown class %q; use one of %s", v, classChoices(allowDocument)))
	}
	return c.Key, nil
}

func classChoices(allowDocument bool) string {
	names := contract.ClassNames()
	if allowDocument {
		names = append(names, corpus.DocumentClass)
	}
	return strings.Join(names, ", ")
}

func classEnum(allowDocument bool) []string {
	names := contract.ClassNames()
	if allowDocument {
		names = append(names, corpus.DocumentClass)
	}
	return names
}

// corpus loads the register, mapping a failure to a tool error.
func (s *service) corpus(ctx context.Context, args map[string]any) (*corpus.Corpus, error) {
	c, err := s.Store.Get(ctx, argBool(args, "refresh"))
	if err != nil {
		return nil, upstream(err)
	}
	return c, nil
}

func corpusWarnings(c *corpus.Corpus) []string {
	var out []string
	if c.Stale {
		out = append(out, fmt.Sprintf("Tesseract could not be refreshed (%s); this is the register as of %s.", c.StaleReason, c.LoadedAt.UTC().Format(time.RFC3339)))
	}
	return append(out, c.Warnings...)
}

// withWarnings adds a warnings key only when there is something to warn about,
// so a clean result does not carry an empty (or null) one.
func withWarnings(m map[string]any, warnings []string) map[string]any {
	if len(warnings) > 0 {
		m["warnings"] = warnings
	}
	return m
}

// row is the compact form of a record in a list or search result. It carries
// enough to choose from; atlas_get returns the whole record.
type row struct {
	ID           string   `json:"id"`
	Class        string   `json:"class"`
	Name         string   `json:"name,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	State        string   `json:"state,omitempty"`
	ReviewStatus string   `json:"review_status,omitempty"`
	Visibility   string   `json:"visibility,omitempty"`
	Systems      []string `json:"systems,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
}

func rowOf(r *corpus.Record) row {
	out := row{
		ID: r.Key, Class: r.Class, Summary: r.Summary, State: r.State,
		ReviewStatus: r.ReviewStatus, Visibility: r.Visibility(), Systems: r.Systems(),
		CreatedAt: r.CreatedAt,
	}
	if r.Name != r.Key {
		out.Name = r.Name
	}
	if r.IsDocument() {
		out.Namespace = r.Namespace
	}
	return out
}

// page is the list envelope: the same count/total/truncated vocabulary as
// go-mcp's budget.Envelope, plus the offset a stateless page needs.
type page struct {
	Items      any      `json:"items"`
	Count      int      `json:"count"`
	Total      int      `json:"total"`
	Offset     int      `json:"offset"`
	NextOffset *int     `json:"next_offset"`
	Truncated  bool     `json:"truncated"`
	Hint       string   `json:"hint,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// paged slices items at offset for limit and fills the envelope.
func paged[T any](items []T, offset, limit int, hint string, warnings []string) page {
	total := len(items)
	offset = min(offset, total)
	end := min(offset+limit, total)
	out := page{Items: items[offset:end], Count: end - offset, Total: total, Offset: offset, Hint: hint, Warnings: warnings}
	if end < total {
		out.NextOffset = &end
		out.Truncated = true
	}
	if out.Items == nil {
		out.Items = []T{}
	}
	return out
}

// classRank orders class keys the way the contract lists them, documents last.
func classRank(class string) int {
	for i, c := range contract.Classes {
		if c.Key == class {
			return i
		}
	}
	return len(contract.Classes)
}
