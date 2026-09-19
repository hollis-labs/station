// Package contract is the machine-readable form of the Atlas contract: the
// seven record classes, the key scheme, each class's lifecycle vocabulary
// and the closed sets the audit checks records against.
//
// It is derived from the ATLAS-META-* records in Tesseract
// (project/atlas/knowledge/meta), which are still drafts under Chrispian's
// review, and from the choices agreed on 2026-09-19 that those records do
// not carry yet. Every vocabulary entry says which of those it is (see
// Term.Status), so a reader of an audit finding can tell "the contract says
// no" from "agreed in session, not yet written down". Tesseract itself
// enforces none of this: it stores whatever `data`, `consumer_state` and tags
// it is given.
package contract

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Root is the knowledge namespace every Atlas record and document lives
// under. Class records sit one level down, in a namespace per class.
const Root = "project/atlas/knowledge"

// SummaryLimit is the summary bound the Initiative template states. Only that
// template says it, so the audit reports a longer summary as ambiguity, not
// as a violation.
const SummaryLimit = 200

// DefaultVisibility is the visibility the contract assumes for a record with
// no visibility tag.
const DefaultVisibility = "portfolio"

// Class is one Atlas record class.
type Class struct {
	Key     string // "use-case"; the value after `atlas:` in the class tag
	Name    string // "Use case"
	Prefix  string // "ATLAS-UC-"
	Segment string // namespace segment under Root: "use-cases"

	// StateField is the consumer_state key that carries this class's own
	// lifecycle vocabulary; empty when the class has none (Working note).
	StateField string
	States     []string

	// Chooser says what question the class answers; NotThis says what it is
	// often confused with. Both feed atlas_guide.
	Chooser string
	NotThis string

	// DataKeys is the template's data shape in order; Required are the keys a
	// record must carry (an empty value is a separate, softer finding).
	DataKeys []string
	Required []string

	// Conventions are data keys in use across the corpus that the contract
	// does not list. They are reported as convention, not as a violation.
	Conventions []string
}

// Namespace is the knowledge namespace holding this class's records.
func (c Class) Namespace() string { return Root + "/" + c.Segment }

// Tag is the class tag every record of the class carries.
func (c Class) Tag() string { return "atlas:" + c.Key }

// Classes lists the seven classes in the contract's order.
var Classes = []Class{
	{
		Key: "initiative", Name: "Initiative", Prefix: "ATLAS-INIT-", Segment: "initiatives",
		StateField: "status",
		States:     []string{"proposed", "active", "paused", "completed", "superseded", "cancelled", "unclear"},
		Chooser:    "a push that spans projects, described by outcome and movement",
		NotThis:    "a task list; Torque holds executable work",
		DataKeys: []string{"id", "name", "outcome", "why_it_matters", "participating_systems", "authoritative_refs",
			"current_evidence", "recent_movement", "open_decisions", "watch_outs", "next_milestone", "relationships", "landed_milestones"},
		Required: []string{"id", "name", "outcome", "why_it_matters", "participating_systems", "current_evidence",
			"recent_movement", "open_decisions", "watch_outs", "relationships"},
	},
	{
		Key: "asset", Name: "Asset", Prefix: "ATLAS-ASSET-", Segment: "assets",
		StateField:  "capability_maturity",
		States:      []string{"local_experiment", "extraction_candidate", "extracted", "stable_primitive", "retired"},
		Chooser:     "a thing that exists: application, service, library, protocol, pattern, workflow, plugin, data model or capability",
		NotThis:     "a project's own README or docs, which stay the authority",
		DataKeys:    []string{"id", "name", "asset_kind", "local_mechanism", "immediate_composition", "portfolio_role", "operator_intent", "canonical_location", "relationships"},
		Required:    []string{"id", "name", "asset_kind", "local_mechanism", "immediate_composition", "portfolio_role", "operator_intent", "relationships"},
		Conventions: []string{"consumption_modes", "notable_strength", "confidence_note"},
	},
	{
		Key: "decision", Name: "Decision", Prefix: "ATLAS-DECISION-", Segment: "decisions",
		StateField: "status",
		States:     []string{"proposed", "decided", "superseded", "reversed"},
		Chooser:    "a cross-project direction chosen, with its rationale",
		NotThis:    "a project's own ADR, which lives in that project's knowledge/adr",
		DataKeys:   []string{"id", "name", "decision", "rationale", "alternatives_considered", "consequences", "scope", "relationships"},
		Required:   []string{"id", "name", "decision", "rationale", "alternatives_considered", "consequences", "scope", "relationships"},
	},
	{
		Key: "note", Name: "Working note", Prefix: "ATLAS-NOTE-", Segment: "notes",
		Chooser:  "thinking that is still forming",
		NotThis:  "a Decision, until it has been decided",
		DataKeys: []string{"id", "name", "thinking", "why_now", "what_would_change_it", "relationships"},
		Required: []string{"id", "name", "thinking", "why_now", "what_would_change_it", "relationships"},
	},
	{
		Key: "use-case", Name: "Use case", Prefix: "ATLAS-UC-", Segment: "use-cases",
		StateField: "maturity",
		States:     []string{"idea", "explored", "prototyped", "working", "adopted", "superseded"},
		Chooser:    "a concrete way assets compose",
		NotThis:    "an Asset",
		DataKeys: []string{"id", "name", "intent", "flow", "initiating_surface", "routing_execution_owner", "context_evidence_required",
			"output_destination", "human_approval_points", "delivery_recovery_expectations", "relationships"},
		Required: []string{"id", "name", "intent", "flow", "initiating_surface", "routing_execution_owner", "context_evidence_required",
			"output_destination", "human_approval_points", "delivery_recovery_expectations", "relationships"},
		Conventions: []string{"scale"},
	},
	{
		Key: "question", Name: "Question", Prefix: "ATLAS-Q-", Segment: "questions",
		StateField: "status",
		States:     []string{"open", "answered", "moot"},
		Chooser:    "an uncertainty that states what would resolve it",
		NotThis:    "a vague worry with no resolution path",
		DataKeys:   []string{"id", "name", "question", "why_it_matters", "what_would_answer_it", "relationships"},
		Required:   []string{"id", "name", "question", "why_it_matters", "what_would_answer_it", "relationships"},
	},
	{
		Key: "followup", Name: "Follow-up", Prefix: "ATLAS-FOLLOWUP-", Segment: "followups",
		StateField: "status",
		States:     []string{"draft", "ready", "promoted", "declined"},
		Chooser:    "candidate work not yet moved to Torque",
		NotThis:    "a Torque task, which is the authority once one exists",
		DataKeys:   []string{"id", "name", "goal", "candidate_owner_project", "expected_outputs", "questions_to_answer", "watch_outs", "promoted_to", "relationships"},
		Required:   []string{"id", "name", "goal", "candidate_owner_project", "expected_outputs", "questions_to_answer", "watch_outs", "promoted_to", "relationships"},
	},
}

// ClassByKey returns the class with the given Key ("use-case").
func ClassByKey(key string) (Class, bool) {
	for _, c := range Classes {
		if c.Key == key {
			return c, true
		}
	}
	return Class{}, false
}

// ClassByName is the forgiving lookup for an argument a person or agent
// typed: it accepts a class key ("use-case"), its display name ("Use case",
// "Working note"), or the key-prefix token ("uc", "q", "init").
func ClassByName(name string) (Class, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	for _, c := range Classes {
		token := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(c.Prefix, "ATLAS-"), "-"))
		if n == c.Key || n == strings.NewReplacer(" ", "-").Replace(strings.ToLower(c.Name)) || n == token {
			return c, true
		}
	}
	return Class{}, false
}

// ClassNames lists the accepted class keys, for schema enums and messages.
func ClassNames() []string {
	names := make([]string, len(Classes))
	for i, c := range Classes {
		names[i] = c.Key
	}
	return names
}

// ClassOfNamespace returns the class whose namespace is exactly ns.
func ClassOfNamespace(ns string) (Class, bool) {
	for _, c := range Classes {
		if c.Namespace() == ns {
			return c, true
		}
	}
	return Class{}, false
}

var idPattern = regexp.MustCompile(`(?i)^ATLAS-([A-Z]+)-(\d+)$`)

// ParseID splits an Atlas key ("ATLAS-Q-014") into its class and number. It
// is case-insensitive and does not require the number to be zero-padded, so
// it accepts what an agent is likely to type; use ValidKey to check a stored
// key against the strict scheme.
func ParseID(id string) (Class, int, bool) {
	m := idPattern.FindStringSubmatch(strings.TrimSpace(id))
	if m == nil {
		return Class{}, 0, false
	}
	token := "ATLAS-" + strings.ToUpper(m[1]) + "-"
	for _, c := range Classes {
		if c.Prefix == token {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				return Class{}, 0, false
			}
			return c, n, true
		}
	}
	return Class{}, 0, false
}

// NormalizeID rewrites an Atlas key typed loosely ("atlas-q-14") into the
// scheme's form ("ATLAS-Q-014"). Anything that is not an Atlas key of a
// known class is returned trimmed and otherwise unchanged, since document
// keys are free-form.
func NormalizeID(id string) string {
	id = strings.TrimSpace(id)
	c, n, ok := ParseID(id)
	if !ok {
		return id
	}
	return fmt.Sprintf("%s%03d", c.Prefix, n)
}

// ValidKey reports whether key follows the strict scheme for class c:
// the prefix, then at least three digits.
func ValidKey(c Class, key string) bool {
	rest, ok := strings.CutPrefix(key, c.Prefix)
	if !ok || len(rest) < 3 {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Term is one entry of a closed vocabulary, with where it stands.
type Term struct {
	Name string
	// Status is "contract" (in the ATLAS-META records), "proposed" (agreed
	// 2026-09-19 but not yet written into them) or "convention" (in use across
	// the corpus, ratified by no one).
	Status string
	Alias  string // the contract term this one stands in for, if any
	Note   string
}

// RelationshipTypes is the closed set for data.relationships[].type.
var RelationshipTypes = []Term{
	{Name: "depends_on", Status: "contract"},
	{Name: "consumes", Status: "contract"},
	{Name: "unlocks", Status: "contract"},
	{Name: "extends", Status: "contract"},
	{Name: "composes_with", Status: "contract"},
	{Name: "informs", Status: "contract"},
	{Name: "supersedes", Status: "contract"},
	{Name: "duplicates", Status: "contract"},
	{Name: "produces", Status: "contract"},
	{Name: "relates_to", Status: "contract", Note: "the honest fallback when no specific verb fits"},
	{Name: "implemented_within", Status: "proposed", Note: "a capability Asset to its parent Asset; agreed 2026-09-19, not yet in ATLAS-META"},
	{Name: "composes", Status: "proposed", Alias: "composes_with", Note: "agreed 2026-09-19 to mean composes_with; not yet in ATLAS-META"},
}

// LookupRelationship finds a relationship type by name.
func LookupRelationship(name string) (Term, bool) {
	for _, t := range RelationshipTypes {
		if t.Name == name {
			return t, true
		}
	}
	return Term{}, false
}

// Visibilities are the values after `visibility:` in a visibility tag.
var Visibilities = []string{"private", "portfolio", "public"}

// AssetKinds is the closed set for an Asset's data.asset_kind.
var AssetKinds = []string{"application", "service", "library", "protocol", "pattern", "workflow", "plugin", "data-model", "capability"}

// ReviewStatuses is the closed set for consumer_state.review_status.
var ReviewStatuses = []string{"draft", "reviewed"}

// ConfidenceLabels maps the qualitative label to the numeric confidence a
// record is written with.
var ConfidenceLabels = []struct {
	Label string
	Value float64
}{
	{"high", 0.9}, {"medium-high", 0.8}, {"medium", 0.6}, {"low / exploratory", 0.4},
}

// ConventionTagDimensions are tag prefixes in use across the corpus that the
// contract does not define. The contract says maturity and status are
// deliberately not tags; these duplicate fields that already carry them.
var ConventionTagDimensions = []string{"maturity:", "asset-kind:", "scale:", "lang:"}

// Tag kinds, as returned by TagKind.
const (
	TagAtlas       = "atlas"
	TagClass       = "class"
	TagVisibility  = "visibility"
	TagMeta        = "meta"
	TagConvention  = "convention"
	TagSystem      = "system"
	TagOtherPrefix = "prefixed"
)

// TagKind classifies a tag: the bare `atlas` tag, a class tag, a visibility
// tag, the `atlas:meta` marker, a convention dimension the contract does not
// define, some other prefixed tag, or a system tag (one per participating
// system, the contract's third dimension).
func TagKind(tag string) string {
	switch {
	case tag == "atlas":
		return TagAtlas
	case tag == "atlas:meta":
		return TagMeta
	case strings.HasPrefix(tag, "atlas:"):
		if _, ok := ClassByKey(strings.TrimPrefix(tag, "atlas:")); ok {
			return TagClass
		}
		return TagOtherPrefix
	case strings.HasPrefix(tag, "visibility:"):
		return TagVisibility
	}
	for _, p := range ConventionTagDimensions {
		if strings.HasPrefix(tag, p) {
			return TagConvention
		}
	}
	if strings.Contains(tag, ":") {
		return TagOtherPrefix
	}
	return TagSystem
}

// VisibilityOf returns the values of a tag list's visibility tags, in order.
func VisibilityOf(tags []string) []string {
	var out []string
	for _, t := range tags {
		if v, ok := strings.CutPrefix(t, "visibility:"); ok {
			out = append(out, v)
		}
	}
	return out
}

// ValidVisibility reports whether v is one of the contract's values.
func ValidVisibility(v string) bool { return slices.Contains(Visibilities, v) }
