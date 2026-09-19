package corpus_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/station/internal/atlas/atlastest"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

func TestBuildClassifiesByNamespace(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Record("question", "ATLAS-Q-001"),
		atlastest.Doc(contract.Root, "atlas/init-014-harnessed-agent-runtimes"),
		atlastest.Doc(contract.Root+"/meta", "ATLAS-META-CHARTER"),
	}, time.Time{})

	if r := c.ByKey["ATLAS-Q-001"]; r == nil || r.Class != "question" || r.IsDocument() {
		t.Errorf("ATLAS-Q-001 = %+v, want a question record", r)
	}
	for _, key := range []string{"atlas/init-014-harnessed-agent-runtimes", "ATLAS-META-CHARTER"} {
		if r := c.ByKey[key]; r == nil || !r.IsDocument() {
			t.Errorf("%s = %+v, want a document", key, r)
		}
	}
	// A record in the META namespace has an ATLAS- key but is not a class record.
	if got := c.ClassRecords(); len(got) != 1 || got[0].Key != "ATLAS-Q-001" {
		t.Errorf("ClassRecords = %v, want only the question", keys(got))
	}
}

func TestBuildOrdersByClassThenNaturalKey(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Doc(contract.Root+"/adr", "adr_x"),
		atlastest.Record("question", "ATLAS-Q-010"),
		atlastest.Record("question", "ATLAS-Q-009"),
		atlastest.Record("initiative", "ATLAS-INIT-002"),
		atlastest.Record("question", "ATLAS-Q-002"),
	}, time.Time{})

	want := []string{"ATLAS-INIT-002", "ATLAS-Q-002", "ATLAS-Q-009", "ATLAS-Q-010", "adr_x"}
	if got := keys(c.Records); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if !corpus.KeyLess("ATLAS-Q-9", "ATLAS-Q-10") {
		t.Error("KeyLess compares digits as text: ATLAS-Q-9 must sort before ATLAS-Q-10")
	}
}

func TestRecordsKeepStoredValuesEvenWhenTheyBreakTheContract(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Record("initiative", "ATLAS-INIT-013",
			atlastest.State(map[string]any{"review_status": "host-split-shipped", "status": "host_library_extracted_hadron_plugin_pending"})),
		atlastest.Record("question", "ATLAS-Q-001", atlastest.NoState()),
	}, time.Time{})

	r := c.ByKey["ATLAS-INIT-013"]
	if r.ReviewStatus != "host-split-shipped" || r.State != "host_library_extracted_hadron_plugin_pending" {
		t.Errorf("review_status=%q state=%q; off-vocabulary values must be kept as stored, not normalized to draft", r.ReviewStatus, r.State)
	}
	if a := c.ByKey["ATLAS-Q-001"]; a.HasReviewStatus || a.ReviewStatus != "" {
		t.Errorf("an absent review_status read as %q (has=%v)", a.ReviewStatus, a.HasReviewStatus)
	}
}

func TestInboundLinksCoverRelationshipsAndWikilinks(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Record("question", "ATLAS-Q-001"),
		atlastest.Record("question", "ATLAS-Q-002",
			atlastest.Relationships(atlastest.Rel("depends_on", "ATLAS-Q-001")),
			atlastest.Body("needs [[ATLAS-Q-001]]")),
		atlastest.Record("note", "ATLAS-NOTE-001", atlastest.Body("see [[ATLAS-Q-001|the first question]]")),
		// The agreed target grammar prefixes an Atlas key with atlas:.
		atlastest.Record("note", "ATLAS-NOTE-002", atlastest.Relationships(atlastest.Rel("informs", "atlas:ATLAS-Q-001"))),
		// A reference to a record that was never written still has inbound links.
		atlastest.Record("note", "ATLAS-NOTE-003", atlastest.Relationships(atlastest.Rel("relates_to", "ATLAS-Q-099"))),
	}, time.Time{})

	type edge struct{ from, via, typ string }
	var got []edge
	for _, l := range c.Inbound("ATLAS-Q-001") {
		got = append(got, edge{l.From.Key, l.Via, l.Type})
	}
	for _, want := range []edge{
		{"ATLAS-Q-002", "relationship", "depends_on"},
		{"ATLAS-Q-002", "wikilink", ""},
		{"ATLAS-NOTE-001", "wikilink", ""},
		{"ATLAS-NOTE-002", "relationship", "informs"},
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Inbound(ATLAS-Q-001) is missing %+v; have %+v", want, got)
		}
	}
	if _, exists := c.ByKey["ATLAS-Q-099"]; exists || len(c.Inbound("ATLAS-Q-099")) != 1 {
		t.Error("a dangling target should have inbound links and no record")
	}
}

func TestResolveAcceptsTheTypedAndLooseForms(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{atlastest.Record("question", "ATLAS-Q-014")}, time.Time{})
	for _, target := range []string{"ATLAS-Q-014", "atlas:ATLAS-Q-014", "atlas-q-14", " ATLAS-Q-014 "} {
		if r, ok := c.Resolve(target); !ok || r.Key != "ATLAS-Q-014" {
			t.Errorf("Resolve(%q) = %v, %v", target, r, ok)
		}
	}
	if _, ok := c.Resolve("tether"); ok {
		t.Error("a bare project slug resolved to a record")
	}
}

func TestSuggestFindsTheRootDocumentThatHoldsAnInitiative(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Record("initiative", "ATLAS-INIT-013"),
		atlastest.Record("initiative", "ATLAS-INIT-015"),
		atlastest.Doc(contract.Root, "atlas/init-014-harnessed-agent-runtimes"),
		atlastest.Doc(contract.Root, "atlas/initiative-register"),
	}, time.Time{})

	got := c.Suggest("ATLAS-INIT-014", 5)
	if !slices.Contains(got, "atlas/init-014-harnessed-agent-runtimes") {
		t.Errorf("Suggest(ATLAS-INIT-014) = %v, want the slug-keyed document", got)
	}
	if slices.Contains(got, "ATLAS-INIT-013") || slices.Contains(got, "ATLAS-INIT-015") {
		t.Errorf("Suggest offered a neighbouring key as if it were the same record: %v", got)
	}
	if got := c.Suggest("x", 5); got != nil {
		t.Errorf("a too-short query suggested %v", got)
	}
}

func TestDuplicateKeysAcrossNamespacesAreRecorded(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Doc(contract.Root+"/adr", "adr_shared"),
		atlastest.Doc(contract.Root+"/meta", "adr_shared"),
	}, time.Time{})
	if len(c.Duplicates["adr_shared"]) != 2 {
		t.Errorf("Duplicates = %v", c.Duplicates)
	}
	if c.ByKey["adr_shared"] == nil {
		t.Error("ByKey lost a duplicated key entirely")
	}
}

func TestRelationshipsThatAreNotObjectsAreKeptAndMarked(t *testing.T) {
	c := corpus.Build([]tesseract.Revision{
		atlastest.Record("note", "ATLAS-NOTE-001", atlastest.Data(map[string]any{"relationships": []any{"ATLAS-Q-001", map[string]any{"type": "informs"}}})),
	}, time.Time{})
	rels := c.ByKey["ATLAS-NOTE-001"].Relationships
	if len(rels) != 2 || !rels[0].Malformed || !rels[1].Malformed {
		t.Errorf("relationships = %+v, want both entries kept and marked malformed", rels)
	}
}

func TestNearDuplicateTagsGroupSpellingsOfOneSystem(t *testing.T) {
	groups := corpus.NearDuplicateTags(map[string]int{
		"agentkit": 16, "agent-kit": 19, "tether": 46, "tesseract": 25,
		"visibility:portfolio": 480, "maturity:extracted": 85,
	})
	if len(groups) != 1 || !slices.Equal(groups[0], []string{"agent-kit", "agentkit"}) {
		t.Errorf("groups = %v, want only agent-kit/agentkit", groups)
	}
}

// --- Store ---

func store(t *testing.T, ttl time.Duration, revs ...tesseract.Revision) (*corpus.Store, *atlastest.Fake) {
	t.Helper()
	fake := atlastest.New(t, revs...)
	return corpus.NewStore(tesseract.New(fake.URL(), ""), ttl), fake
}

func TestStoreLoadsTheRootNamespaceAsWellAsItsChildren(t *testing.T) {
	// The recall glob covers the sub-namespaces and not the root itself, which
	// holds documents of its own; the store must ask for both.
	s, fake := store(t, time.Hour,
		atlastest.Record("question", "ATLAS-Q-001"),
		atlastest.Doc(contract.Root, "atlas/init-014-x"),
		atlastest.Doc(contract.Root+"/meta", "ATLAS-META-CHARTER"),
	)
	c, err := s.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ATLAS-Q-001", "atlas/init-014-x", "ATLAS-META-CHARTER"} {
		if c.ByKey[key] == nil {
			t.Errorf("%s was not loaded", key)
		}
	}
	if n := fake.Calls("recall"); n != 1 {
		t.Errorf("recall calls = %d, want the whole register in one paged read", n)
	}
}

func TestStoreCachesWithinTTLAndRefreshBypassesIt(t *testing.T) {
	s, fake := store(t, time.Hour, atlastest.Record("question", "ATLAS-Q-001"))
	bg := context.Background()
	if _, err := s.Get(bg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(bg, false); err != nil {
		t.Fatal(err)
	}
	if n := fake.Calls("recall"); n != 1 {
		t.Fatalf("recall calls = %d after two cached reads, want 1", n)
	}

	fake.Set(atlastest.Record("question", "ATLAS-Q-001"), atlastest.Record("question", "ATLAS-Q-002"))
	c, err := s.Get(bg, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.ByKey["ATLAS-Q-002"] == nil || fake.Calls("recall") != 2 {
		t.Errorf("refresh=true did not re-read Tesseract (calls=%d)", fake.Calls("recall"))
	}
}

func TestStoreWithZeroTTLNeverCaches(t *testing.T) {
	s, fake := store(t, 0, atlastest.Record("question", "ATLAS-Q-001"))
	for range 3 {
		if _, err := s.Get(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.Calls("recall"); n != 3 {
		t.Errorf("recall calls = %d, want 3", n)
	}
}

func TestStoreServesTheLastCorpusMarkedStaleWhenARefreshFails(t *testing.T) {
	s, fake := store(t, time.Hour, atlastest.Record("question", "ATLAS-Q-001"))
	if _, err := s.Get(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	fake.Close()

	c, err := s.Get(context.Background(), true)
	if err != nil {
		t.Fatalf("a failed refresh with a corpus in hand returned %v; an older answer that says it is older beats none", err)
	}
	if !c.Stale || c.StaleReason == "" || c.ByKey["ATLAS-Q-001"] == nil {
		t.Errorf("stale=%v reason=%q: the fallback must say it is stale and still carry the records", c.Stale, c.StaleReason)
	}
}

func TestStoreFirstLoadFailureIsAnError(t *testing.T) {
	s, fake := store(t, time.Hour, atlastest.Record("question", "ATLAS-Q-001"))
	fake.Close()
	if _, err := s.Get(context.Background(), false); !errors.Is(err, tesseract.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func keys(rs []*corpus.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Key
	}
	return out
}
