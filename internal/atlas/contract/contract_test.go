package contract_test

import (
	"testing"

	"github.com/hollis-labs/station/internal/atlas/contract"
)

func TestParseIDAndNormalizeID(t *testing.T) {
	tests := []struct {
		in    string
		class string
		num   int
		ok    bool
		norm  string
	}{
		{"ATLAS-Q-014", "question", 14, true, "ATLAS-Q-014"},
		{"atlas-q-14", "question", 14, true, "ATLAS-Q-014"},
		{"  ATLAS-UC-3 ", "use-case", 3, true, "ATLAS-UC-003"},
		{"ATLAS-FOLLOWUP-013", "followup", 13, true, "ATLAS-FOLLOWUP-013"},
		{"ATLAS-ASSET-0674", "asset", 674, true, "ATLAS-ASSET-674"},
		// ATLAS-TASK- was the drafts' earlier prefix for what is now a Follow-up.
		{"ATLAS-TASK-001", "", 0, false, "ATLAS-TASK-001"},
		// A bare UC-025 is a draft-local number, not a key.
		{"UC-025", "", 0, false, "UC-025"},
		// Document keys are free-form and pass through untouched.
		{"atlas/init-014-harnessed-agent-runtimes", "", 0, false, "atlas/init-014-harnessed-agent-runtimes"},
	}
	for _, tt := range tests {
		c, n, ok := contract.ParseID(tt.in)
		if ok != tt.ok || (ok && (c.Key != tt.class || n != tt.num)) {
			t.Errorf("ParseID(%q) = %q, %d, %v; want %q, %d, %v", tt.in, c.Key, n, ok, tt.class, tt.num, tt.ok)
		}
		if got := contract.NormalizeID(tt.in); got != tt.norm {
			t.Errorf("NormalizeID(%q) = %q, want %q", tt.in, got, tt.norm)
		}
	}
}

func TestValidKeyIsStrict(t *testing.T) {
	q, _ := contract.ClassByKey("question")
	tests := map[string]bool{
		"ATLAS-Q-014":   true,
		"ATLAS-Q-0140":  true,
		"ATLAS-Q-14":    false, // fewer than three digits
		"ATLAS-Q-01x":   false,
		"ATLAS-UC-014":  false, // another class's prefix
		"ATLAS-Q-":      false,
		"":              false,
		"atlas-q-014":   false, // stored keys are exact; only lookups forgive case
		"ATLAS-Q-014-b": false,
	}
	for key, want := range tests {
		if got := contract.ValidKey(q, key); got != want {
			t.Errorf("ValidKey(question, %q) = %v, want %v", key, got, want)
		}
	}
}

func TestClassByNameIsForgiving(t *testing.T) {
	tests := map[string]string{
		"use-case":     "use-case",
		"Use case":     "use-case",
		"uc":           "use-case",
		"Q":            "question",
		"question":     "question",
		"Working note": "note",
		"note":         "note",
		"init":         "initiative",
		"Follow-up":    "followup",
		"followup":     "followup",
		"Asset":        "asset",
	}
	for in, want := range tests {
		c, ok := contract.ClassByName(in)
		if !ok || c.Key != want {
			t.Errorf("ClassByName(%q) = %q, %v; want %q", in, c.Key, ok, want)
		}
	}
	if _, ok := contract.ClassByName("task"); ok {
		t.Error(`ClassByName("task") matched; the class was renamed to follow-up`)
	}
}

func TestClassOfNamespace(t *testing.T) {
	if c, ok := contract.ClassOfNamespace(contract.Root + "/questions"); !ok || c.Key != "question" {
		t.Errorf("questions namespace = %q, %v", c.Key, ok)
	}
	for _, ns := range []string{contract.Root, contract.Root + "/meta", contract.Root + "/adr", "project/tether/knowledge/questions"} {
		if c, ok := contract.ClassOfNamespace(ns); ok {
			t.Errorf("%q is not a class namespace but matched %q", ns, c.Key)
		}
	}
}

func TestEveryClassNamespaceAndTagFollowsItsKey(t *testing.T) {
	for _, c := range contract.Classes {
		if c.Namespace() != contract.Root+"/"+c.Segment {
			t.Errorf("%s namespace = %q", c.Key, c.Namespace())
		}
		if c.Tag() != "atlas:"+c.Key {
			t.Errorf("%s tag = %q", c.Key, c.Tag())
		}
		// A class with a lifecycle field has a vocabulary, and one without has none.
		if (c.StateField == "") != (len(c.States) == 0) {
			t.Errorf("%s: state field %q with %d states", c.Key, c.StateField, len(c.States))
		}
	}
}

func TestRelationshipVocabularyStatus(t *testing.T) {
	tests := []struct {
		name, status, alias string
		ok                  bool
	}{
		{"depends_on", "contract", "", true},
		{"relates_to", "contract", "", true},
		{"implemented_within", "proposed", "", true},
		{"composes", "proposed", "composes_with", true},
		{"wraps", "", "", false},
	}
	for _, tt := range tests {
		term, ok := contract.LookupRelationship(tt.name)
		if ok != tt.ok || term.Status != tt.status || term.Alias != tt.alias {
			t.Errorf("LookupRelationship(%q) = %+v, %v", tt.name, term, ok)
		}
	}
}

func TestTagKind(t *testing.T) {
	tests := map[string]string{
		"atlas":                 contract.TagAtlas,
		"atlas:question":        contract.TagClass,
		"atlas:use-case":        contract.TagClass,
		"atlas:meta":            contract.TagMeta,
		"atlas:bogus":           contract.TagOtherPrefix,
		"visibility:portfolio":  contract.TagVisibility,
		"maturity:extracted":    contract.TagConvention,
		"asset-kind:capability": contract.TagConvention,
		"scale:large":           contract.TagConvention,
		"lang:typescript":       contract.TagConvention,
		"go-agent-wrapper":      contract.TagSystem,
		"agentkit":              contract.TagSystem,
		"project:atlas":         contract.TagOtherPrefix,
	}
	for tag, want := range tests {
		if got := contract.TagKind(tag); got != want {
			t.Errorf("TagKind(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestVisibilityOf(t *testing.T) {
	got := contract.VisibilityOf([]string{"atlas", "visibility:private", "x", "visibility:public"})
	if len(got) != 2 || got[0] != "private" || got[1] != "public" {
		t.Errorf("VisibilityOf = %v", got)
	}
	if !contract.ValidVisibility("portfolio") || contract.ValidVisibility("secret") {
		t.Error("ValidVisibility disagrees with the contract's three values")
	}
}
