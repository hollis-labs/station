// Package audit checks the Atlas register against the contract and reports
// where they disagree. It never repairs anything.
//
// Every finding says what it is measured against (its Basis), because the
// contract is a draft and the corpus has moved faster than it:
//
//   - contract:   the ATLAS-META records say otherwise, plainly.
//   - convention: matches something in use across the corpus (or agreed on
//     2026-09-19) that the contract does not carry yet.
//   - ambiguity:  the contract is silent, or disagrees with itself.
//   - observed:   drift the contract does not speak to, such as a link to a
//     record that was never written.
//
// Whether a disagreement means the record or the contract is wrong is
// Chrispian's call, so a finding is evidence, not a verdict. The same
// vocabulary tables that drive the write tools (when they exist) drive these
// rules, so "valid" has one definition.
package audit

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
)

// Bases and severities.
const (
	BasisContract   = "contract"
	BasisConvention = "convention"
	BasisAmbiguity  = "ambiguity"
	BasisObserved   = "observed"

	SeverityError = "error"
	SeverityWarn  = "warn"
	SeverityInfo  = "info"
)

// Rule describes one check.
type Rule struct {
	ID    string
	Title string
	// Basis and Severity are the defaults; a finding may carry different ones
	// (a document is held to less than a class record).
	Basis    string
	Severity string
}

// Catalog lists every rule, so a report can say what was checked, including
// the rules that found nothing.
var Catalog = []Rule{
	{"key-scheme", "Key does not follow ATLAS-<CLASS>-NNN for its class", BasisContract, SeverityError},
	{"tag-atlas", "Missing the atlas or atlas:<class> tag", BasisContract, SeverityError},
	{"visibility-tag", "Visibility tag missing, repeated or not one of private/portfolio/public", BasisContract, SeverityWarn},
	{"tag-format", "System tag is not lowercase-hyphenated", BasisContract, SeverityWarn},
	{"tag-convention", "Tag in a dimension the contract does not define (maturity:, asset-kind:, scale:, lang:)", BasisConvention, SeverityInfo},
	{"tag-near-duplicate", "Two spellings of one system tag", BasisObserved, SeverityInfo},
	{"review-status", "review_status absent or not draft/reviewed", BasisContract, SeverityError},
	{"last-reviewed-on-draft", "last_reviewed set on a record that has not been reviewed", BasisAmbiguity, SeverityInfo},
	{"state-missing", "Class lifecycle field absent", BasisContract, SeverityWarn},
	{"state-vocab", "Class lifecycle value outside the class vocabulary", BasisContract, SeverityError},
	{"asset-kind", "Asset asset_kind outside the closed set", BasisContract, SeverityWarn},
	{"data-missing", "Required data key absent", BasisContract, SeverityWarn},
	{"data-empty", "Required data field present but empty", BasisAmbiguity, SeverityInfo},
	{"data-extra-key", "data key the template does not list", BasisAmbiguity, SeverityInfo},
	{"data-id", "data.id differs from the key", BasisContract, SeverityWarn},
	{"summary-empty", "Summary empty", BasisContract, SeverityError},
	{"summary-length", "Summary longer than the Initiative template's 200 characters", BasisAmbiguity, SeverityInfo},
	{"rel-type", "Relationship type outside the closed set", BasisContract, SeverityWarn},
	{"rel-malformed", "Relationship entry is not {type, target}", BasisContract, SeverityError},
	{"rel-target-missing", "Relationship names an Atlas key that no record has", BasisContract, SeverityWarn},
	{"rel-no-wikilink", "Relationship to an Atlas record has no [[wikilink]] in the body, so Tesseract's link graph cannot walk it", BasisContract, SeverityWarn},
	{"wikilink-dangling", "[[wikilink]] names no record", BasisObserved, SeverityWarn},
	{"draft-local-ref", "Bare UC-nnn in the body: a draft-local number, not a key", BasisObserved, SeverityInfo},
	{"question-answered-unlinked", "Answered Question has no relationship to the record that answered it", BasisContract, SeverityWarn},
	{"decision-superseded-unlinked", "Superseded Decision has no successor linked with a supersedes relationship", BasisContract, SeverityWarn},
	{"followup-promotion", "Follow-up status and promoted_to disagree", BasisContract, SeverityWarn},
	{"initiative-at-root", "Initiative-shaped document at the root under a slug key, with no ATLAS-INIT record", BasisObserved, SeverityWarn},
	{"key-duplicate", "Same key stored in more than one knowledge namespace", BasisObserved, SeverityWarn},
}

// Finding is one disagreement between a record and what it is measured
// against.
type Finding struct {
	Rule     string
	Basis    string
	Severity string
	Key      string // "" for a finding about the corpus as a whole
	Class    string
	Detail   string
}

// RuleByID returns the catalog entry for id.
func RuleByID(id string) (Rule, bool) {
	for _, r := range Catalog {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

var (
	draftLocalRef = regexp.MustCompile(`(?:^|[^A-Za-z0-9-])UC-\d{3}\b`)
	atlasKeyLike  = regexp.MustCompile(`(?i)^ATLAS-[A-Z]+-\d+$`)
	initInSlug    = regexp.MustCompile(`(?i)\binit-(\d{3})\b`)
	systemTag     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Run checks every record and returns the findings, ordered by rule and key.
func Run(c *corpus.Corpus) []Finding {
	var out []Finding
	emit := func(rule string, r *corpus.Record, detail string) {
		f := Finding{Rule: rule, Detail: detail}
		if def, ok := RuleByID(rule); ok {
			f.Basis, f.Severity = def.Basis, def.Severity
		}
		if r != nil {
			f.Key, f.Class = r.Key, r.Class
		}
		out = append(out, f)
	}
	// override emits with a basis and severity that differ from the rule's default.
	override := func(rule string, r *corpus.Record, basis, severity, detail string) {
		emit(rule, r, detail)
		last := &out[len(out)-1]
		last.Basis, last.Severity = basis, severity
	}

	for _, r := range c.Records {
		checkTags(r, emit, override)
		checkReview(r, emit, override)
		checkSummary(r, emit)
		checkBody(c, r, emit)
		checkRelationships(c, r, emit, override)
		if r.IsDocument() {
			checkDocument(c, r, emit)
			continue
		}
		checkClassRecord(c, r, emit, override)
	}
	for key, rs := range c.Duplicates {
		var spaces []string
		for _, r := range rs {
			spaces = append(spaces, r.Namespace)
		}
		emit("key-duplicate", rs[0], fmt.Sprintf("%q is stored in %s; lookups by key return the first", key, strings.Join(spaces, ", ")))
	}
	checkNearDuplicateTags(c, emit)

	ruleOrder := map[string]int{}
	for i, r := range Catalog {
		ruleOrder[r.ID] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ruleOrder[out[i].Rule] != ruleOrder[out[j].Rule] {
			return ruleOrder[out[i].Rule] < ruleOrder[out[j].Rule]
		}
		return corpus.KeyLess(out[i].Key, out[j].Key)
	})
	return out
}

type emitFn func(rule string, r *corpus.Record, detail string)
type overrideFn func(rule string, r *corpus.Record, basis, severity, detail string)

func checkTags(r *corpus.Record, emit emitFn, override overrideFn) {
	if !r.IsDocument() {
		def, _ := r.ClassDef()
		if !r.HasTag("atlas") {
			emit("tag-atlas", r, "missing the atlas tag")
		}
		if !r.HasTag(def.Tag()) {
			emit("tag-atlas", r, fmt.Sprintf("missing the class tag %s", def.Tag()))
		}
	}

	vis := contract.VisibilityOf(r.Tags)
	switch {
	case len(vis) == 0 && r.IsDocument():
		override("visibility-tag", r, BasisAmbiguity, SeverityInfo, "a document with no visibility tag; the contract defines tags for class records only")
	case len(vis) == 0:
		emit("visibility-tag", r, "no visibility tag (the contract's default is portfolio, but every record is meant to carry one)")
	case len(vis) > 1:
		emit("visibility-tag", r, "more than one visibility tag: "+strings.Join(vis, ", "))
	}
	for _, v := range vis {
		if !contract.ValidVisibility(v) {
			emit("visibility-tag", r, fmt.Sprintf("visibility:%s is not one of %s", v, strings.Join(contract.Visibilities, ", ")))
		}
	}

	var conventions []string
	for _, t := range r.Tags {
		switch contract.TagKind(t) {
		case contract.TagSystem:
			if !systemTag.MatchString(t) {
				emit("tag-format", r, fmt.Sprintf("%q: system tags are lowercase and hyphenated", t))
			}
		case contract.TagConvention:
			conventions = append(conventions, t)
		}
	}
	if len(conventions) > 0 {
		emit("tag-convention", r, strings.Join(conventions, ", ")+" (the contract says maturity and status are deliberately not tags)")
	}
}

func checkReview(r *corpus.Record, emit emitFn, override overrideFn) {
	if !r.HasReviewStatus {
		if r.IsDocument() {
			override("review-status", r, BasisAmbiguity, SeverityInfo, "a document with no review_status; the contract defines the review lifecycle for class records only")
		} else {
			emit("review-status", r, "consumer_state.review_status is absent")
		}
	} else if !slices.Contains(contract.ReviewStatuses, r.ReviewStatus) {
		emit("review-status", r, fmt.Sprintf("review_status %q is not one of %s", r.ReviewStatus, strings.Join(contract.ReviewStatuses, ", ")))
	}
	if r.LastReviewed != "" && r.ReviewStatus != "reviewed" {
		emit("last-reviewed-on-draft", r, fmt.Sprintf("last_reviewed is %q but the record has not been reviewed; the contract defines it as the review date, and its own template shows it on a draft", r.LastReviewed))
	}
}

func checkSummary(r *corpus.Record, emit emitFn) {
	switch n := len([]rune(r.Summary)); {
	case n == 0:
		emit("summary-empty", r, "summary is empty")
	case n > contract.SummaryLimit && !r.IsDocument():
		emit("summary-length", r, fmt.Sprintf("summary is %d characters", n))
	}
}

func checkBody(c *corpus.Corpus, r *corpus.Record, emit emitFn) {
	for _, w := range r.Wikilinks {
		if _, ok := c.Resolve(w); !ok {
			emit("wikilink-dangling", r, fmt.Sprintf("[[%s]] names no record", w))
		}
	}
	if !r.IsDocument() && draftLocalRef.MatchString(r.Body) {
		emit("draft-local-ref", r, "the body cites a bare UC-nnn; cite the full key (ATLAS-UC-nnn)")
	}
}

func checkRelationships(c *corpus.Corpus, r *corpus.Record, emit emitFn, override overrideFn) {
	for _, rel := range r.Relationships {
		if rel.Malformed {
			emit("rel-malformed", r, fmt.Sprintf("entry %q is not an object with a string type and target", rel.Target))
			continue
		}
		if term, ok := contract.LookupRelationship(rel.Type); !ok {
			emit("rel-type", r, fmt.Sprintf("type %q is outside the closed set (target %s)", rel.Type, rel.Target))
		} else if term.Status != "contract" {
			override("rel-type", r, BasisConvention, SeverityInfo, fmt.Sprintf("type %q: %s", rel.Type, term.Note))
		}

		target := corpus.NormalizeTarget(rel.Target)
		if !atlasKeyLike.MatchString(target) {
			continue
		}
		if _, ok := c.ByKey[target]; !ok {
			emit("rel-target-missing", r, fmt.Sprintf("%s names %s, which no record has", rel.Type, target))
			continue
		}
		if !slices.ContainsFunc(r.Wikilinks, func(w string) bool { return corpus.NormalizeTarget(w) == target }) {
			emit("rel-no-wikilink", r, fmt.Sprintf("%s %s has no [[%s]] in the body", rel.Type, target, target))
		}
	}
}

func checkDocument(c *corpus.Corpus, r *corpus.Record, emit emitFn) {
	if r.Namespace != contract.Root {
		return
	}
	m := initInSlug.FindStringSubmatch(r.Key)
	if m == nil {
		return
	}
	want := "ATLAS-INIT-" + m[1]
	if _, ok := c.ByKey[want]; !ok {
		emit("initiative-at-root", r, fmt.Sprintf("%q reads as %s but sits at the namespace root; no record exists at %s, so links to that key resolve nowhere", r.Key, want, want))
	}
}

func checkClassRecord(c *corpus.Corpus, r *corpus.Record, emit emitFn, override overrideFn) {
	def, ok := r.ClassDef()
	if !ok {
		return
	}
	if !contract.ValidKey(def, r.Key) {
		emit("key-scheme", r, fmt.Sprintf("%q does not follow %sNNN", r.Key, def.Prefix))
	}
	if id := text(r.Data["id"]); id != "" && id != r.Key {
		emit("data-id", r, fmt.Sprintf("data.id is %q but the key is %q", id, r.Key))
	}

	if def.StateField != "" {
		switch {
		case r.State == "":
			emit("state-missing", r, fmt.Sprintf("consumer_state.%s is absent or empty", def.StateField))
		case !slices.Contains(def.States, r.State):
			emit("state-vocab", r, fmt.Sprintf("%s %q is not one of %s", def.StateField, r.State, strings.Join(def.States, ", ")))
		}
	}

	var missing, empty []string
	for _, k := range def.Required {
		v, present := r.Data[k]
		switch {
		case !present:
			missing = append(missing, k)
		case k != "relationships" && k != "promoted_to" && isEmpty(v):
			empty = append(empty, k)
		}
	}
	if len(missing) > 0 {
		emit("data-missing", r, "no "+strings.Join(missing, ", "))
	}
	if len(empty) > 0 {
		emit("data-empty", r, "empty: "+strings.Join(empty, ", "))
	}

	var extra, known []string
	for k := range r.Data {
		if slices.Contains(def.DataKeys, k) {
			continue
		}
		if slices.Contains(def.Conventions, k) {
			known = append(known, k)
		} else {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(known)
	if len(extra) > 0 {
		emit("data-extra-key", r, "not in the template: "+strings.Join(extra, ", "))
	}
	if len(known) > 0 {
		override("data-extra-key", r, BasisConvention, SeverityInfo, "in use across the corpus but not in the template: "+strings.Join(known, ", "))
	}

	if r.Class == "asset" {
		if kind := text(r.Data["asset_kind"]); !slices.Contains(contract.AssetKinds, kind) {
			emit("asset-kind", r, fmt.Sprintf("asset_kind %q is not one of %s", kind, strings.Join(contract.AssetKinds, ", ")))
		}
	}

	switch r.Class {
	case "question":
		if r.State == "answered" && !answered(c, r) {
			emit("question-answered-unlinked", r, "answered, but no relationship points at the Decision or record that answered it")
		}
	case "decision":
		if r.State == "superseded" && !hasSuccessor(c, r) {
			emit("decision-superseded-unlinked", r, "superseded, but no record links to it with a supersedes relationship")
		}
	case "followup":
		promotedTo := text(r.Data["promoted_to"])
		switch {
		case r.State == "promoted" && promotedTo == "":
			emit("followup-promotion", r, "status is promoted but data.promoted_to is empty")
		case r.State != "promoted" && promotedTo != "":
			emit("followup-promotion", r, fmt.Sprintf("data.promoted_to is %q but status is %q", promotedTo, r.State))
		}
	}
}

// answered reports whether an answered Question links to something other than
// another Question: the record that settled it.
func answered(c *corpus.Corpus, q *corpus.Record) bool {
	for _, rel := range q.Relationships {
		if target, ok := c.Resolve(rel.Target); ok && target.Class != "question" {
			return true
		}
	}
	return false
}

// hasSuccessor reports whether some record links to d with a supersedes
// relationship, which is how the contract says a replaced Decision is marked.
func hasSuccessor(c *corpus.Corpus, d *corpus.Record) bool {
	for _, l := range c.Inbound(d.Key) {
		if l.Via == "relationship" && l.Type == "supersedes" {
			return true
		}
	}
	return false
}

func checkNearDuplicateTags(c *corpus.Corpus, emit emitFn) {
	counts := c.TagCounts()
	for _, group := range corpus.NearDuplicateTags(counts) {
		parts := make([]string, len(group))
		for i, t := range group {
			parts[i] = fmt.Sprintf("%s (%d)", t, counts[t])
		}
		emit("tag-near-duplicate", nil, strings.Join(parts, " ~ ")+": a filter on one misses the other")
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	}
	return false
}

func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
