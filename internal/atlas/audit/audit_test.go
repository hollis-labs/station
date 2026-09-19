package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/station/internal/atlas/atlastest"
	"github.com/hollis-labs/station/internal/atlas/audit"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

func run(revs ...tesseract.Revision) []audit.Finding {
	return audit.Run(corpus.Build(revs, time.Time{}))
}

func find(fs []audit.Finding, rule, key string) (audit.Finding, bool) {
	for _, f := range fs {
		if f.Rule == rule && f.Key == key {
			return f, true
		}
	}
	return audit.Finding{}, false
}

func expect(t *testing.T, fs []audit.Finding, rule, key string) audit.Finding {
	t.Helper()
	f, ok := find(fs, rule, key)
	if !ok {
		t.Fatalf("no %q finding for %s; findings: %v", rule, key, describe(fs))
	}
	return f
}

func expectNone(t *testing.T, fs []audit.Finding, rule, key string) {
	t.Helper()
	if f, ok := find(fs, rule, key); ok {
		t.Errorf("unexpected %q finding for %s: %s", rule, key, f.Detail)
	}
}

func describe(fs []audit.Finding) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.Rule+"@"+f.Key)
	}
	return strings.Join(parts, " ")
}

// question is a fully conforming Question; options override it.
func question(key string, opts ...atlastest.Opt) tesseract.Revision {
	base := []atlastest.Opt{
		atlastest.Data(map[string]any{"question": "Q?", "why_it_matters": "because", "what_would_answer_it": "a test"}),
		atlastest.State(map[string]any{"status": "open"}),
	}
	return atlastest.Record("question", key, append(base, opts...)...)
}

func TestAConformingRecordHasNoFindings(t *testing.T) {
	for _, f := range run(question("ATLAS-Q-001")) {
		if f.Key == "ATLAS-Q-001" {
			t.Errorf("finding on a conforming record: %s %s", f.Rule, f.Detail)
		}
	}
}

func TestEachRuleFiresOnItsCase(t *testing.T) {
	tests := []struct {
		name  string
		rule  string
		basis string
		rev   tesseract.Revision
	}{
		{"key without the class scheme", "key-scheme", audit.BasisContract, question("ATLAS-Q-14")},
		{"missing atlas tag", "tag-atlas", audit.BasisContract, question("ATLAS-Q-002", atlastest.Tags("atlas:question", "visibility:portfolio"))},
		{"missing class tag", "tag-atlas", audit.BasisContract, question("ATLAS-Q-002", atlastest.Tags("atlas", "visibility:portfolio"))},
		{"no visibility tag", "visibility-tag", audit.BasisContract, question("ATLAS-Q-002", atlastest.Tags("atlas", "atlas:question"))},
		{"two visibility tags", "visibility-tag", audit.BasisContract, question("ATLAS-Q-002", atlastest.AddTags("visibility:private"))},
		{"visibility outside the set", "visibility-tag", audit.BasisContract, question("ATLAS-Q-002", atlastest.Tags("atlas", "atlas:question", "visibility:secret"))},
		{"system tag not hyphenated", "tag-format", audit.BasisContract, question("ATLAS-Q-002", atlastest.AddTags("Go_Agent"))},
		{"convention tag dimension", "tag-convention", audit.BasisConvention, question("ATLAS-Q-002", atlastest.AddTags("maturity:extracted"))},
		{"review status outside draft/reviewed", "review-status", audit.BasisContract, question("ATLAS-Q-002", atlastest.State(map[string]any{"review_status": "host-split-shipped"}))},
		{"last_reviewed on a draft", "last-reviewed-on-draft", audit.BasisAmbiguity, question("ATLAS-Q-002", atlastest.State(map[string]any{"last_reviewed": "2026-09-16"}))},
		{"lifecycle value empty", "state-missing", audit.BasisContract, question("ATLAS-Q-002", atlastest.State(map[string]any{"status": ""}))},
		// active is an Initiative word and means nothing on a Question.
		{"another class's lifecycle word", "state-vocab", audit.BasisContract, question("ATLAS-Q-002", atlastest.State(map[string]any{"status": "active"}))},
		{"required data keys absent", "data-missing", audit.BasisContract, atlastest.Record("question", "ATLAS-Q-002", atlastest.State(map[string]any{"status": "open"}))},
		{"required field empty", "data-empty", audit.BasisAmbiguity, question("ATLAS-Q-002", atlastest.Data(map[string]any{"why_it_matters": ""}))},
		{"data key the template lacks", "data-extra-key", audit.BasisAmbiguity, question("ATLAS-Q-002", atlastest.Data(map[string]any{"weird": 1}))},
		{"data.id differs from the key", "data-id", audit.BasisContract, question("ATLAS-Q-002", atlastest.Data(map[string]any{"id": "ATLAS-Q-999"}))},
		{"empty summary", "summary-empty", audit.BasisContract, question("ATLAS-Q-002", atlastest.Summary(""))},
		{"summary over 200 characters", "summary-length", audit.BasisAmbiguity, question("ATLAS-Q-002", atlastest.Summary(strings.Repeat("x", 201)))},
		{"free-text asset_kind", "asset-kind", audit.BasisContract, atlastest.Record("asset", "ATLAS-ASSET-001",
			atlastest.Data(map[string]any{"asset_kind": "application / agent framework"}),
			atlastest.State(map[string]any{"capability_maturity": "extracted"}))},
		{"convention data key on an asset", "data-extra-key", audit.BasisConvention, atlastest.Record("asset", "ATLAS-ASSET-002",
			atlastest.Data(map[string]any{"asset_kind": "capability", "consumption_modes": []any{"library"}}),
			atlastest.State(map[string]any{"capability_maturity": "extracted"}))},
		{"promoted follow-up with no promoted_to", "followup-promotion", audit.BasisContract, atlastest.Record("followup", "ATLAS-FOLLOWUP-001",
			atlastest.State(map[string]any{"status": "promoted"}))},
		{"promoted_to on a follow-up that is not promoted", "followup-promotion", audit.BasisContract, atlastest.Record("followup", "ATLAS-FOLLOWUP-002",
			atlastest.Data(map[string]any{"promoted_to": "CW-20260918-0054"}),
			atlastest.State(map[string]any{"status": "draft"}))},
		{"draft-local UC number", "draft-local-ref", audit.BasisObserved, question("ATLAS-Q-002", atlastest.Body("compare UC-025 first"))},
		{"dangling wikilink", "wikilink-dangling", audit.BasisObserved, question("ATLAS-Q-002", atlastest.Body("see [[ATLAS-NOTE-028]]"))},
		{"placeholder wikilink", "wikilink-dangling", audit.BasisObserved, question("ATLAS-Q-002", atlastest.Body("use [[wikilink]] for targets"))},
		{"relationship that is not an object", "rel-malformed", audit.BasisContract, question("ATLAS-Q-002", atlastest.Data(map[string]any{"relationships": []any{"ATLAS-Q-001"}}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := expect(t, run(tt.rev), tt.rule, tt.rev.MemoryKey)
			if f.Basis != tt.basis {
				t.Errorf("basis = %q, want %q (%s)", f.Basis, tt.basis, f.Detail)
			}
			if f.Detail == "" {
				t.Error("a finding with no detail cannot be acted on")
			}
		})
	}
}

func TestBareUCNumberIsFlaggedButAFullKeyIsNot(t *testing.T) {
	fs := run(
		question("ATLAS-Q-001", atlastest.Body("compare UC-025")),
		question("ATLAS-Q-002", atlastest.Body("compare ATLAS-UC-025")),
	)
	expect(t, fs, "draft-local-ref", "ATLAS-Q-001")
	expectNone(t, fs, "draft-local-ref", "ATLAS-Q-002")
}

func TestRelationshipTypesAreJudgedByWhereTheyStand(t *testing.T) {
	target := question("ATLAS-Q-010")
	src := func(typ string) []audit.Finding {
		return run(target, question("ATLAS-Q-011",
			atlastest.Relationships(atlastest.Rel(typ, "ATLAS-Q-010")),
			atlastest.Body("[[ATLAS-Q-010]]")))
	}

	expectNone(t, src("depends_on"), "rel-type", "ATLAS-Q-011")
	expectNone(t, src("relates_to"), "rel-type", "ATLAS-Q-011")

	// Outside every list: a contract finding.
	if f := expect(t, src("wraps"), "rel-type", "ATLAS-Q-011"); f.Basis != audit.BasisContract {
		t.Errorf("wraps basis = %q, want contract", f.Basis)
	}
	// Agreed on 2026-09-19 but not yet in the contract: reported as convention,
	// so it reads as "not written down yet" and not as a defect.
	for _, typ := range []string{"implemented_within", "composes"} {
		f := expect(t, src(typ), "rel-type", "ATLAS-Q-011")
		if f.Basis != audit.BasisConvention || f.Severity != audit.SeverityInfo {
			t.Errorf("%s: basis=%q severity=%q, want convention/info", typ, f.Basis, f.Severity)
		}
	}
}

func TestRelationshipTargetsAndTheirWikilinks(t *testing.T) {
	target := question("ATLAS-Q-010")
	check := func(rel map[string]any, body string) []audit.Finding {
		return run(target, question("ATLAS-Q-011", atlastest.Relationships(rel), atlastest.Body(body)))
	}

	fs := check(atlastest.Rel("depends_on", "ATLAS-Q-010"), "needs [[ATLAS-Q-010]]")
	expectNone(t, fs, "rel-no-wikilink", "ATLAS-Q-011")
	expectNone(t, fs, "rel-target-missing", "ATLAS-Q-011")

	// The array is display only: without the wikilink Tesseract's link graph cannot walk the edge.
	expect(t, check(atlastest.Rel("depends_on", "ATLAS-Q-010"), "no link in the prose"), "rel-no-wikilink", "ATLAS-Q-011")

	// The agreed typed grammar resolves, and a wikilink to the bare key satisfies it.
	fs = check(atlastest.Rel("depends_on", "atlas:ATLAS-Q-010"), "needs [[ATLAS-Q-010]]")
	expectNone(t, fs, "rel-no-wikilink", "ATLAS-Q-011")
	expectNone(t, fs, "rel-target-missing", "ATLAS-Q-011")

	// A key that looks like an Atlas key and resolves to nothing.
	fs = check(atlastest.Rel("depends_on", "ATLAS-Q-099"), "")
	expect(t, fs, "rel-target-missing", "ATLAS-Q-011")
	expectNone(t, fs, "rel-no-wikilink", "ATLAS-Q-011")

	// An external reference is not held to the Atlas-key rules.
	fs = check(atlastest.Rel("informs", "project/hadron"), "")
	expectNone(t, fs, "rel-target-missing", "ATLAS-Q-011")
	expectNone(t, fs, "rel-no-wikilink", "ATLAS-Q-011")
}

func TestAnAnsweredQuestionMustPointAtWhatAnsweredIt(t *testing.T) {
	decision := atlastest.Record("decision", "ATLAS-DECISION-001", atlastest.State(map[string]any{"status": "decided"}))
	answered := func(key string, rels ...map[string]any) tesseract.Revision {
		return question(key, atlastest.State(map[string]any{"status": "answered"}), atlastest.Relationships(rels...))
	}
	fs := run(decision,
		question("ATLAS-Q-001", atlastest.State(map[string]any{"status": "open"})),
		answered("ATLAS-Q-002"),
		answered("ATLAS-Q-003", atlastest.Rel("relates_to", "ATLAS-DECISION-001")),
		question("ATLAS-Q-005"),
		answered("ATLAS-Q-004", atlastest.Rel("relates_to", "ATLAS-Q-005")),
	)
	expectNone(t, fs, "question-answered-unlinked", "ATLAS-Q-001") // open
	expect(t, fs, "question-answered-unlinked", "ATLAS-Q-002")     // no link at all
	expectNone(t, fs, "question-answered-unlinked", "ATLAS-Q-003") // linked to the deciding record
	expect(t, fs, "question-answered-unlinked", "ATLAS-Q-004")     // linked only to another Question
}

func TestASupersededDecisionNeedsItsSuccessorLinked(t *testing.T) {
	old := atlastest.Record("decision", "ATLAS-DECISION-001", atlastest.State(map[string]any{"status": "superseded"}))
	orphan := atlastest.Record("decision", "ATLAS-DECISION-002", atlastest.State(map[string]any{"status": "superseded"}))
	successor := atlastest.Record("decision", "ATLAS-DECISION-003",
		atlastest.State(map[string]any{"status": "decided"}),
		atlastest.Relationships(atlastest.Rel("supersedes", "ATLAS-DECISION-001")))

	fs := run(old, orphan, successor)
	expectNone(t, fs, "decision-superseded-unlinked", "ATLAS-DECISION-001")
	expect(t, fs, "decision-superseded-unlinked", "ATLAS-DECISION-002")
	expectNone(t, fs, "decision-superseded-unlinked", "ATLAS-DECISION-003")
}

func TestAnInitiativeShapedDocumentAtTheRootIsFlaggedUntilItsRecordExists(t *testing.T) {
	doc := atlastest.Doc(contract.Root, "atlas/init-014-harnessed-agent-runtimes")

	f := expect(t, run(doc), "initiative-at-root", doc.MemoryKey)
	if !strings.Contains(f.Detail, "ATLAS-INIT-014") {
		t.Errorf("detail %q should name the key that resolves nowhere", f.Detail)
	}
	expectNone(t, run(doc, atlastest.Record("initiative", "ATLAS-INIT-014")), "initiative-at-root", doc.MemoryKey)
	// Only the root is checked; the same slug in a document namespace is not an initiative.
	expectNone(t, run(atlastest.Doc(contract.Root+"/investigations", "init-014-notes")), "initiative-at-root", "init-014-notes")
}

func TestDocumentsAreHeldToLessThanClassRecords(t *testing.T) {
	doc := atlastest.Doc(contract.Root+"/adr", "adr_api_to_mcp_projection")
	fs := run(doc)

	if f := expect(t, fs, "visibility-tag", doc.MemoryKey); f.Basis != audit.BasisAmbiguity || f.Severity != audit.SeverityInfo {
		t.Errorf("visibility on a document: basis=%q severity=%q; the contract defines tags for class records only", f.Basis, f.Severity)
	}
	if f := expect(t, fs, "review-status", doc.MemoryKey); f.Basis != audit.BasisAmbiguity {
		t.Errorf("review-status on a document: basis=%q, want ambiguity", f.Basis)
	}
	for _, rule := range []string{"key-scheme", "tag-atlas", "state-missing", "state-vocab", "data-missing", "data-empty", "data-extra-key", "data-id", "summary-length"} {
		expectNone(t, fs, rule, doc.MemoryKey)
	}
}

func TestTheSameKeyInTwoNamespacesIsReported(t *testing.T) {
	fs := run(atlastest.Doc(contract.Root+"/adr", "adr_shared"), atlastest.Doc(contract.Root+"/meta", "adr_shared"))
	expect(t, fs, "key-duplicate", "adr_shared")
}

func TestNearDuplicateTagsAreACorpusFinding(t *testing.T) {
	fs := run(
		question("ATLAS-Q-001", atlastest.AddTags("agentkit")),
		question("ATLAS-Q-002", atlastest.AddTags("agent-kit")),
	)
	f := expect(t, fs, "tag-near-duplicate", "")
	if !strings.Contains(f.Detail, "agentkit") || !strings.Contains(f.Detail, "agent-kit") {
		t.Errorf("detail = %q, want both spellings", f.Detail)
	}
}

func TestFindingsAreOrderedByRuleThenKey(t *testing.T) {
	fs := run(
		question("ATLAS-Q-10", atlastest.Summary("")),
		question("ATLAS-Q-9", atlastest.Summary("")),
	)
	var keys []string
	for _, f := range fs {
		if f.Rule == "summary-empty" {
			keys = append(keys, f.Key)
		}
	}
	if len(keys) != 2 || keys[0] != "ATLAS-Q-9" || keys[1] != "ATLAS-Q-10" {
		t.Errorf("summary-empty order = %v, want natural key order", keys)
	}
}

func TestEveryRuleInTheCatalogIsKnownAndHasABasis(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range audit.Catalog {
		if seen[r.ID] {
			t.Errorf("rule %q is listed twice", r.ID)
		}
		seen[r.ID] = true
		if r.Title == "" || r.Basis == "" || r.Severity == "" {
			t.Errorf("rule %+v is missing a title, basis or severity", r)
		}
		if got, ok := audit.RuleByID(r.ID); !ok || got.ID != r.ID {
			t.Errorf("RuleByID(%q) = %+v, %v", r.ID, got, ok)
		}
	}
}
