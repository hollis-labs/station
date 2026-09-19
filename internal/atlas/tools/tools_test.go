package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-mcp/budget"
	gmcpserver "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/atlastest"
	"github.com/hollis-labs/station/internal/atlas/audit"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
	"github.com/hollis-labs/station/internal/atlas/tools"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

type env struct {
	srv  *gmcpserver.Server
	fake *atlastest.Fake
}

func setup(t *testing.T, revs ...tesseract.Revision) env {
	t.Helper()
	fake := atlastest.New(t, revs...)
	client := tesseract.New(fake.URL(), "")
	srv := gmcpserver.NewServer("atlas-test", "0")
	tools.Register(srv, tools.Deps{Client: client, Store: corpus.NewStore(client, time.Hour), Now: func() time.Time { return now }})
	return env{srv, fake}
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not an object: %s", b)
	}
	return m
}

func (e env) call(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := e.srv.CallTool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return toMap(t, res)
}

func (e env) text(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	res, err := e.srv.CallTool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	s, ok := res.(string)
	if !ok {
		t.Fatalf("%s returned %T, want text", name, res)
	}
	return s
}

func (e env) fail(t *testing.T, name string, args map[string]any) *budget.ToolError {
	t.Helper()
	_, err := e.srv.CallTool(context.Background(), name, args)
	if err == nil {
		t.Fatalf("%s %v succeeded, want an error", name, args)
	}
	var te *budget.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("%s %v: error %T (%v) is not a tool error an agent can act on", name, args, err, err)
	}
	return te
}

func items(m map[string]any) []map[string]any {
	list, _ := m["items"].([]any)
	out := make([]map[string]any, len(list))
	for i, v := range list {
		out[i], _ = v.(map[string]any)
	}
	return out
}

func ids(m map[string]any) []string {
	var out []string
	for _, it := range items(m) {
		out = append(out, it["id"].(string))
	}
	return out
}

func num(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}

func q(key, status string, opts ...atlastest.Opt) tesseract.Revision {
	base := []atlastest.Opt{
		atlastest.Data(map[string]any{"question": "Q?", "why_it_matters": "because", "what_would_answer_it": "a test"}),
		atlastest.State(map[string]any{"status": status}),
	}
	return atlastest.Record("question", key, append(base, opts...)...)
}

// --- the surface itself ---

func TestEveryToolIsReadOnlyAndTakesNoNamespace(t *testing.T) {
	e := setup(t)
	defs := e.srv.ToolDefinitions()
	if len(defs) == 0 {
		t.Fatal("no tools registered")
	}
	registered := map[string]bool{}
	for _, d := range defs {
		registered[d.Name] = true
		if !strings.HasPrefix(d.Name, "atlas_") {
			t.Errorf("%s: tool names are atlas_-prefixed", d.Name)
		}
		if !d.Annotations.ReadOnlyHint || d.Annotations.DestructiveHint {
			t.Errorf("%s: annotations %+v; the read slice never writes", d.Name, d.Annotations)
		}
		if strings.TrimSpace(d.Description) == "" {
			t.Errorf("%s has no description; the description is the process documentation", d.Name)
		}
		props := d.InputSchema.(map[string]interface{})["properties"].(map[string]interface{})
		for _, forbidden := range []string{"namespace", "domain", "namespaces"} {
			if _, ok := props[forbidden]; ok {
				t.Errorf("%s takes %q; every tool is auto-scoped to the Atlas namespace", d.Name, forbidden)
			}
		}
	}
	for _, name := range tools.Names() {
		if !registered[name] {
			t.Errorf("Names() lists %s but it is not registered", name)
		}
	}
	for _, name := range []string{"atlas_guide", "atlas_get", "atlas_search", "atlas_list"} {
		if !registered[name] {
			t.Errorf("%s is not registered", name)
		}
	}
}

func TestArgumentsAreGuarded(t *testing.T) {
	e := setup(t, q("ATLAS-Q-001", "open"))

	te := e.fail(t, "atlas_list", map[string]any{"class": "question", "clas": "x"})
	if te.Code != "invalid_argument" || te.Field != "clas" || !strings.Contains(te.Message, "accepts:") || te.HelpTool != "atlas_guide" {
		t.Errorf("misspelled argument: %+v; it must be refused with what is accepted, not silently ignored", te)
	}
	if te := e.fail(t, "atlas_get", map[string]any{}); te.Code != "invalid_argument" || te.Field != "id" {
		t.Errorf("missing id: %+v", te)
	}
	if te := e.fail(t, "atlas_list", map[string]any{"class": "banana"}); te.Code != "invalid_argument" || !strings.Contains(te.Message, "question") {
		t.Errorf("unknown class: %+v; the message should list the classes", te)
	}
	if te := e.fail(t, "atlas_list", map[string]any{}); te.Field != "class" {
		t.Errorf("list without class: %+v", te)
	}
	if te := e.fail(t, "atlas_get", map[string]any{"id": "ATLAS-Q-001", "include": []any{"nonsense"}}); te.Field != "include" {
		t.Errorf("bad include: %+v", te)
	}
}

// --- atlas_get ---

func TestGetResolvesRelationshipsAndKeepsStoredValues(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open", atlastest.Data(map[string]any{"name": "First question"})),
		atlastest.Record("initiative", "ATLAS-INIT-013",
			atlastest.State(map[string]any{"status": "active", "review_status": "host-split-shipped"}),
			atlastest.Relationships(
				atlastest.Rel("depends_on", "ATLAS-Q-001"),
				atlastest.Rel("implemented_within", "ATLAS-Q-001"),
				atlastest.Rel("wraps", "ATLAS-Q-404"),
				atlastest.Rel("informs", "project/tether")),
			atlastest.Body("needs [[ATLAS-Q-001]]")),
	)

	got := e.call(t, "atlas_get", map[string]any{"id": "atlas-init-13"})
	if got["id"] != "ATLAS-INIT-013" || got["class"] != "initiative" || got["state_field"] != "status" || got["state"] != "active" {
		t.Errorf("record = %v", got)
	}
	if got["review_status"] != "host-split-shipped" {
		t.Errorf("review_status = %v; an off-vocabulary value must be shown as stored", got["review_status"])
	}

	byTarget := map[string]map[string]any{}
	for _, r := range got["relationships"].([]any) {
		rel := r.(map[string]any)
		byTarget[rel["type"].(string)+"→"+rel["target"].(string)] = rel
	}
	checks := []struct {
		key        string
		resolved   bool
		vocabulary string
	}{
		{"depends_on→ATLAS-Q-001", true, "contract"},
		{"implemented_within→ATLAS-Q-001", true, "proposed"},
		{"wraps→ATLAS-Q-404", false, "unknown"},
		{"informs→project/tether", false, "contract"},
	}
	for _, c := range checks {
		rel := byTarget[c.key]
		if rel == nil || rel["resolved"] != c.resolved || rel["vocabulary"] != c.vocabulary {
			t.Errorf("%s = %v, want resolved=%v vocabulary=%s", c.key, rel, c.resolved, c.vocabulary)
		}
	}
	if rel := byTarget["depends_on→ATLAS-Q-001"]; rel["target_class"] != "question" || rel["target_name"] != "First question" {
		t.Errorf("a resolved target should carry its class and name: %v", rel)
	}
}

func TestGetComputesBacklinksAndFindingsOnlyOnRequest(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open", atlastest.Tags("atlas", "atlas:question")), // no visibility tag
		q("ATLAS-Q-002", "open",
			atlastest.Relationships(atlastest.Rel("depends_on", "ATLAS-Q-001")),
			atlastest.Body("[[ATLAS-Q-001]]")),
	)

	plain := e.call(t, "atlas_get", map[string]any{"id": "ATLAS-Q-001"})
	if plain["backlinks"] != nil || plain["findings"] != nil {
		t.Errorf("backlinks/findings present without include: %v", plain)
	}

	full := e.call(t, "atlas_get", map[string]any{"id": "ATLAS-Q-001", "include": []any{"backlinks", "findings"}})
	back, _ := full["backlinks"].([]any)
	if len(back) != 1 || back[0].(map[string]any)["id"] != "ATLAS-Q-002" {
		t.Errorf("backlinks = %v", full["backlinks"])
	}
	var rules []string
	for _, f := range full["findings"].([]any) {
		rules = append(rules, f.(map[string]any)["rule"].(string))
	}
	if !slices.Contains(rules, "visibility-tag") {
		t.Errorf("findings = %v, want the missing visibility tag", rules)
	}
}

func TestGetNotFoundSaysWhatExistsAndWhereTheRecordMayBe(t *testing.T) {
	e := setup(t,
		atlastest.Record("initiative", "ATLAS-INIT-013"),
		atlastest.Record("initiative", "ATLAS-INIT-015"),
		atlastest.Doc(contract.Root, "atlas/init-014-harnessed-agent-runtimes"),
	)
	te := e.fail(t, "atlas_get", map[string]any{"id": "ATLAS-INIT-014"})
	if te.Code != "not_found" || te.HelpTool != "atlas_guide" {
		t.Fatalf("error = %+v", te)
	}
	if !strings.Contains(te.Message, "ATLAS-INIT-013 through ATLAS-INIT-015") {
		t.Errorf("message = %q, want the range the class holds", te.Message)
	}
	if !strings.Contains(te.NextStep, "atlas/init-014-harnessed-agent-runtimes") || !strings.Contains(te.NextStep, "atlas_list class=initiative") {
		t.Errorf("next step = %q, want the slug-keyed document and how to list the class", te.NextStep)
	}
}

func TestGetFindsDocumentsByKey(t *testing.T) {
	e := setup(t,
		atlastest.Doc(contract.Root, "atlas/init-014-harnessed-agent-runtimes", atlastest.Summary("the initiative dossier")),
		atlastest.Doc(contract.Root+"/meta", "ATLAS-META-CHARTER"),
	)
	for key, ns := range map[string]string{
		"atlas/init-014-harnessed-agent-runtimes": contract.Root,
		"ATLAS-META-CHARTER":                      contract.Root + "/meta",
	} {
		got := e.call(t, "atlas_get", map[string]any{"id": key})
		if got["class"] != "document" || got["namespace"] != ns || got["id"] != key {
			t.Errorf("get %s = %v", key, got)
		}
	}
}

// --- atlas_list ---

func TestListIsExhaustiveAndPagesByOffset(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-010", "open"), q("ATLAS-Q-002", "open"), q("ATLAS-Q-001", "open"),
		q("ATLAS-Q-005", "moot"), q("ATLAS-Q-003", "answered"), q("ATLAS-Q-004", "open"),
		atlastest.Record("note", "ATLAS-NOTE-001"),
	)

	first := e.call(t, "atlas_list", map[string]any{"class": "question", "limit": 4})
	if got := ids(first); !slices.Equal(got, []string{"ATLAS-Q-001", "ATLAS-Q-002", "ATLAS-Q-003", "ATLAS-Q-004"}) {
		t.Errorf("first page = %v, want natural key order", got)
	}
	if num(first, "total") != 6 || first["truncated"] != true || num(first, "next_offset") != 4 {
		t.Errorf("first page envelope = %v", first)
	}

	second := e.call(t, "atlas_list", map[string]any{"class": "question", "limit": 4, "offset": 4})
	if got := ids(second); !slices.Equal(got, []string{"ATLAS-Q-005", "ATLAS-Q-010"}) {
		t.Errorf("second page = %v", got)
	}
	if second["next_offset"] != nil || second["truncated"] != false {
		t.Errorf("last page must say it is the last: next_offset=%v truncated=%v", second["next_offset"], second["truncated"])
	}
}

func TestListFiltersOnStateReviewStatusSystemAndVisibility(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open", atlastest.AddTags("tether")),
		q("ATLAS-Q-002", "answered", atlastest.AddTags("tether")),
		q("ATLAS-Q-003", "open", atlastest.State(map[string]any{"review_status": "reviewed"})),
		q("ATLAS-Q-004", "open", atlastest.Tags("atlas", "atlas:question")),
		q("ATLAS-Q-005", "open", atlastest.Tags("atlas", "atlas:question", "visibility:private")),
	)
	cases := []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"state": "open"}, []string{"ATLAS-Q-001", "ATLAS-Q-003", "ATLAS-Q-004", "ATLAS-Q-005"}},
		{map[string]any{"system": "tether"}, []string{"ATLAS-Q-001", "ATLAS-Q-002"}},
		{map[string]any{"review_status": "reviewed"}, []string{"ATLAS-Q-003"}},
		{map[string]any{"visibility": "untagged"}, []string{"ATLAS-Q-004"}},
		{map[string]any{"visibility": "private"}, []string{"ATLAS-Q-005"}},
		{map[string]any{"state": "open", "system": "tether"}, []string{"ATLAS-Q-001"}},
	}
	for _, c := range cases {
		args := map[string]any{"class": "question"}
		for k, v := range c.args {
			args[k] = v
		}
		if got := ids(e.call(t, "atlas_list", args)); !slices.Equal(got, c.want) {
			t.Errorf("filter %v = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestListAcceptsLooseClassNamesAndListsDocuments(t *testing.T) {
	e := setup(t,
		atlastest.Record("use-case", "ATLAS-UC-001"),
		q("ATLAS-Q-001", "open"),
		atlastest.Doc(contract.Root+"/adr", "adr_x"),
	)
	for _, name := range []string{"uc", "Use case", "use-case"} {
		if got := ids(e.call(t, "atlas_list", map[string]any{"class": name})); !slices.Equal(got, []string{"ATLAS-UC-001"}) {
			t.Errorf("class %q = %v", name, got)
		}
	}
	docs := e.call(t, "atlas_list", map[string]any{"class": "document"})
	if got := ids(docs); !slices.Equal(got, []string{"adr_x"}) {
		t.Errorf("documents = %v", got)
	}
	if items(docs)[0]["namespace"] != contract.Root+"/adr" {
		t.Errorf("a document row should say which namespace it is in: %v", items(docs)[0])
	}
}

// --- atlas_search ---

func alphaCorpus() []tesseract.Revision {
	var revs []tesseract.Revision
	for i := 1; i <= 12; i++ {
		status := "open"
		if i%3 == 0 {
			status = "answered"
		}
		revs = append(revs, q(fmt.Sprintf("ATLAS-Q-%03d", i), status, atlastest.Summary(fmt.Sprintf("alpha question %d", i))))
	}
	revs = append(revs,
		atlastest.Record("asset", "ATLAS-ASSET-001", atlastest.Summary("alpha asset")),
		atlastest.Doc(contract.Root, "atlas/alpha-doc", atlastest.Summary("alpha document")),
		atlastest.Record("note", "ATLAS-NOTE-001", atlastest.Summary("unrelated")),
	)
	return revs
}

func TestSearchScopesToAClassAndPagesWithTheCursor(t *testing.T) {
	e := setup(t, alphaCorpus()...)
	var seen []string
	args := map[string]any{"query": "alpha", "class": "question", "limit": 5}
	for page := 0; page < 5; page++ {
		res := e.call(t, "atlas_search", args)
		for _, it := range items(res) {
			if it["class"] != "question" {
				t.Errorf("class=question search returned %v", it)
			}
			seen = append(seen, it["id"].(string))
		}
		cursor, _ := res["next_cursor"].(string)
		if cursor == "" {
			if num(res, "total") != 12 {
				t.Errorf("total = %v, want the full 12 matches", res["total"])
			}
			break
		}
		args["cursor"] = cursor
	}
	if len(seen) != 12 {
		t.Errorf("paging collected %d questions, want all 12: %v", len(seen), seen)
	}
}

func TestSearchAcrossTheRegisterIncludesRootDocuments(t *testing.T) {
	e := setup(t, alphaCorpus()...)
	res := e.call(t, "atlas_search", map[string]any{"query": "alpha", "limit": 50})
	got := ids(res)
	if !slices.Contains(got, "atlas/alpha-doc") || !slices.Contains(got, "ATLAS-ASSET-001") || slices.Contains(got, "ATLAS-NOTE-001") {
		t.Errorf("results = %v", got)
	}
	for _, it := range items(res) {
		if it["id"] == "atlas/alpha-doc" && it["class"] != "document" {
			t.Errorf("a root document should read as a document: %v", it)
		}
	}
}

func TestSearchFiltersAreAppliedToTheRankedWindowAndSayHowMuchWasScanned(t *testing.T) {
	e := setup(t, alphaCorpus()...)
	res := e.call(t, "atlas_search", map[string]any{"query": "alpha", "class": "question", "state": "answered", "limit": 10})
	if got := ids(res); !slices.Equal(got, []string{"ATLAS-Q-003", "ATLAS-Q-006", "ATLAS-Q-009", "ATLAS-Q-012"}) {
		t.Errorf("answered questions = %v", got)
	}
	if num(res, "scanned") != 12 {
		t.Errorf("scanned = %v; a filtered search must say how many ranked matches it looked at", res["scanned"])
	}
	if _, has := res["next_cursor"]; has {
		t.Error("a filtered search has no cursor to follow")
	}

	docs := e.call(t, "atlas_search", map[string]any{"query": "alpha", "class": "document"})
	if got := ids(docs); !slices.Equal(got, []string{"atlas/alpha-doc"}) {
		t.Errorf("document search = %v", got)
	}

	te := e.fail(t, "atlas_search", map[string]any{"query": "alpha", "state": "open", "cursor": "abc"})
	if te.Code != "invalid_argument" || te.Field != "cursor" {
		t.Errorf("cursor with a filter: %+v", te)
	}
}

func TestSearchCapturesAreOptIn(t *testing.T) {
	capture := atlastest.Doc("project/atlas/memory/decisions", "capability_is_asset_kind", atlastest.Summary("alpha capture"))
	capture.Domain = "memory"
	e := setup(t, append(alphaCorpus(), capture)...)

	if slices.Contains(ids(e.call(t, "atlas_search", map[string]any{"query": "alpha", "limit": 50})), "capability_is_asset_kind") {
		t.Error("session captures are not register records and must not appear by default")
	}
	res := e.call(t, "atlas_search", map[string]any{"query": "alpha", "include_captures": true, "limit": 50})
	var found map[string]any
	for _, it := range items(res) {
		if it["id"] == "capability_is_asset_kind" {
			found = it
		}
	}
	if found == nil || found["class"] != "capture" {
		t.Errorf("capture = %v, want it labelled as a capture", found)
	}
	if te := e.fail(t, "atlas_search", map[string]any{"query": "alpha", "class": "question", "include_captures": true}); te.Field != "include_captures" {
		t.Errorf("captures with a class: %+v", te)
	}
}

// --- atlas_backlinks ---

func TestBacklinksCoverRelationshipsWikilinksAndMissingTargets(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open"),
		q("ATLAS-Q-002", "open", atlastest.Relationships(atlastest.Rel("depends_on", "ATLAS-Q-001")), atlastest.Body("[[ATLAS-Q-001]]")),
		atlastest.Record("note", "ATLAS-NOTE-001", atlastest.Body("see [[ATLAS-Q-001]]")),
		atlastest.Record("initiative", "ATLAS-INIT-015", atlastest.Relationships(atlastest.Rel("extends", "ATLAS-INIT-014")), atlastest.Body("[[ATLAS-INIT-014]]")),
	)

	res := e.call(t, "atlas_backlinks", map[string]any{"id": "ATLAS-Q-001"})
	if res["exists"] != true || num(res, "total") != 2 {
		t.Fatalf("backlinks = %v", res)
	}
	byID := map[string]map[string]any{}
	for _, it := range items(res) {
		byID[it["id"].(string)] = it
	}
	if q2 := byID["ATLAS-Q-002"]; q2 == nil || !slices.Contains(toStrings(q2["via"]), "relationship") || !slices.Contains(toStrings(q2["via"]), "wikilink") || !slices.Contains(toStrings(q2["types"]), "depends_on") {
		t.Errorf("ATLAS-Q-002 = %v", q2)
	}
	if byID["ATLAS-NOTE-001"] == nil {
		t.Errorf("a wikilink-only reference is missing: %v", byID)
	}

	if got := ids(e.call(t, "atlas_backlinks", map[string]any{"id": "ATLAS-Q-001", "type": "depends_on"})); !slices.Equal(got, []string{"ATLAS-Q-002"}) {
		t.Errorf("type filter = %v", got)
	}
	if got := ids(e.call(t, "atlas_backlinks", map[string]any{"id": "ATLAS-Q-001", "class": "note"})); !slices.Equal(got, []string{"ATLAS-NOTE-001"}) {
		t.Errorf("class filter = %v", got)
	}

	// The record was never written, and something cites it anyway.
	missing := e.call(t, "atlas_backlinks", map[string]any{"id": "ATLAS-INIT-014"})
	if missing["exists"] != false || !slices.Equal(ids(missing), []string{"ATLAS-INIT-015"}) || missing["note"] == nil {
		t.Errorf("dangling backlinks = %v", missing)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// --- atlas_review_queue ---

func TestReviewQueueListsTheOldestUnreviewedFirstAndCountsRawStatuses(t *testing.T) {
	reviewed := atlastest.State(map[string]any{"review_status": "reviewed"})
	e := setup(t,
		q("ATLAS-Q-001", "open", atlastest.Created("2026-09-17T12:00:00Z")),
		q("ATLAS-Q-002", "open", atlastest.Created("2026-09-15T12:00:00Z")),
		q("ATLAS-Q-003", "open", atlastest.Created("2026-09-10T12:00:00Z"), reviewed),
		atlastest.Record("initiative", "ATLAS-INIT-013", atlastest.Created("2026-09-16T12:00:00Z"),
			atlastest.State(map[string]any{"review_status": "host-split-shipped"})),
		atlastest.Doc(contract.Root+"/meta", "ATLAS-META-CHARTER", atlastest.Created("2026-09-01T00:00:00Z")),
	)
	res := e.call(t, "atlas_review_queue", map[string]any{})

	if got := ids(res); !slices.Equal(got, []string{"ATLAS-Q-002", "ATLAS-INIT-013", "ATLAS-Q-001"}) {
		t.Errorf("queue = %v, want oldest first, reviewed and documents excluded", got)
	}
	if first := items(res)[0]; num(first, "age_days") != 4 {
		t.Errorf("age_days = %v, want 4 (2026-09-15 to 2026-09-19)", first["age_days"])
	}
	statuses := res["by_review_status"].(map[string]any)
	if num(statuses, "draft") != 2 || num(statuses, "reviewed") != 1 || num(statuses, "host-split-shipped") != 1 {
		t.Errorf("by_review_status = %v; an off-vocabulary value must be counted as itself", statuses)
	}
	byClass := res["by_class"].(map[string]any)["question"].(map[string]any)
	if num(byClass, "total") != 3 || num(byClass, "unreviewed") != 2 {
		t.Errorf("by_class[question] = %v", byClass)
	}
	if _, has := res["warnings"]; has {
		t.Errorf("a clean result carries warnings: %v", res["warnings"])
	}
	if got := ids(e.call(t, "atlas_review_queue", map[string]any{"class": "question"})); !slices.Equal(got, []string{"ATLAS-Q-002", "ATLAS-Q-001"}) {
		t.Errorf("class-filtered queue = %v", got)
	}
}

// --- atlas_audit ---

func TestAuditSummarisesThenDrillsDown(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open"),
		q("ATLAS-Q-002", "open", atlastest.Tags("atlas", "atlas:question")),
		q("ATLAS-Q-003", "open", atlastest.Tags("atlas", "atlas:question"), atlastest.AddTags("maturity:extracted")),
	)
	sum := e.call(t, "atlas_audit", map[string]any{})
	if num(sum, "findings") == 0 || num(sum, "records") != 3 {
		t.Fatalf("summary = %v", sum)
	}
	if sum["clean_rules"] == nil {
		t.Error("an unfiltered summary should say which rules found nothing")
	}
	var vis map[string]any
	for _, r := range sum["rules"].([]any) {
		if rule := r.(map[string]any); rule["rule"] == "visibility-tag" {
			vis = rule
		}
	}
	if vis == nil || num(vis, "count") != 2 || len(vis["samples"].([]any)) > 2 {
		t.Errorf("visibility-tag summary = %v, want 2 findings and at most 2 samples", vis)
	}

	drill := e.call(t, "atlas_audit", map[string]any{"rule": "visibility-tag"})
	if got := ids(drill); !slices.Equal(got, []string{"ATLAS-Q-002", "ATLAS-Q-003"}) {
		t.Errorf("drill-down = %v", got)
	}
	one := e.call(t, "atlas_audit", map[string]any{"id": "atlas-q-3"})
	var rules []string
	for _, it := range items(one) {
		rules = append(rules, it["rule"].(string))
	}
	if !slices.Contains(rules, "visibility-tag") || !slices.Contains(rules, "tag-convention") {
		t.Errorf("findings for ATLAS-Q-003 = %v", rules)
	}

	conv := e.call(t, "atlas_audit", map[string]any{"basis": "convention"})
	if by := conv["by_basis"].(map[string]any); len(by) != 1 || num(by, "convention") != 1 {
		t.Errorf("basis=convention summary by_basis = %v", by)
	}
	if conv["clean_rules"] != nil {
		t.Error("a filtered summary must not claim rules are clean")
	}

	if te := e.fail(t, "atlas_audit", map[string]any{"rule": "no-such-rule"}); te.Field != "rule" || !strings.Contains(te.Message, "visibility-tag") {
		t.Errorf("unknown rule: %+v", te)
	}
}

// --- atlas_tags ---

func TestTagsClassifyCountAndFindNearDuplicates(t *testing.T) {
	e := setup(t,
		q("ATLAS-Q-001", "open", atlastest.AddTags("agentkit", "tether", "maturity:extracted")),
		q("ATLAS-Q-002", "open", atlastest.AddTags("agent-kit", "tether")),
		q("ATLAS-Q-003", "open", atlastest.AddTags("agent-kit")),
	)
	res := e.call(t, "atlas_tags", map[string]any{})
	kinds := map[string]string{}
	counts := map[string]int{}
	for _, it := range items(res) {
		kinds[it["tag"].(string)] = it["kind"].(string)
		counts[it["tag"].(string)] = num(it, "count")
	}
	if kinds["atlas"] != "contract" || kinds["visibility:portfolio"] != "contract" || kinds["tether"] != "system" || kinds["maturity:extracted"] != "convention" {
		t.Errorf("kinds = %v", kinds)
	}
	if counts["agent-kit"] != 2 || counts["tether"] != 2 {
		t.Errorf("counts = %v", counts)
	}
	dups := res["near_duplicates"].([]any)
	if len(dups) != 1 || len(dups[0].([]any)) != 2 {
		t.Errorf("near_duplicates = %v, want agentkit/agent-kit", dups)
	}

	if got := items(e.call(t, "atlas_tags", map[string]any{"kind": "system", "prefix": "agent"})); len(got) != 2 {
		t.Errorf("kind+prefix filter = %v", got)
	}

	near := e.call(t, "atlas_tags", map[string]any{"near": "agent_kit"})
	if near["exists"] != false || len(near["near"].([]any)) == 0 || !strings.Contains(near["advice"].(string), "similar") {
		t.Errorf("near agent_kit = %v", near)
	}
	fresh := e.call(t, "atlas_tags", map[string]any{"near": "brand-new-system"})
	if fresh["exists"] != false || len(fresh["near"].([]any)) != 0 {
		t.Errorf("near a genuinely new tag = %v", fresh)
	}
}

// --- atlas_guide ---

func TestGuideIsGeneratedFromTheContract(t *testing.T) {
	e := setup(t)

	overview := e.text(t, "atlas_guide", nil)
	for _, c := range contract.Classes {
		if !strings.Contains(overview, c.Prefix) {
			t.Errorf("the overview omits the %s class (%s)", c.Key, c.Prefix)
		}
	}
	if !strings.Contains(overview, "no write tools yet") {
		t.Error("the overview must say this server does not write yet")
	}

	if q := e.text(t, "atlas_guide", map[string]any{"topic": "class:uc"}); !strings.Contains(q, "ATLAS-UC-") || !strings.Contains(q, "maturity") {
		t.Errorf("class:uc guide = %q", q)
	}
	vocab := e.text(t, "atlas_guide", map[string]any{"topic": "vocabularies"})
	for _, term := range contract.RelationshipTypes {
		if !strings.Contains(vocab, term.Name) {
			t.Errorf("the vocabularies topic omits relationship type %s", term.Name)
		}
	}
	auditGuide := e.text(t, "atlas_guide", map[string]any{"topic": "audit"})
	for _, r := range audit.Catalog {
		if !strings.Contains(auditGuide, r.ID) {
			t.Errorf("the audit topic omits rule %s", r.ID)
		}
	}
	toolsGuide := e.text(t, "atlas_guide", map[string]any{"topic": "tools"})
	for _, name := range tools.Names() {
		if !strings.Contains(toolsGuide, name) {
			t.Errorf("the tools topic omits %s", name)
		}
	}
	for _, topic := range []string{"classes", "tags", "lifecycle", "boundaries"} {
		if strings.TrimSpace(e.text(t, "atlas_guide", map[string]any{"topic": topic})) == "" {
			t.Errorf("topic %s is empty", topic)
		}
	}

	te := e.fail(t, "atlas_guide", map[string]any{"topic": "nonsense"})
	if te.Code != "invalid_argument" || !strings.Contains(te.Message, "lifecycle") {
		t.Errorf("unknown topic: %+v; it should list the topics", te)
	}
	if te := e.fail(t, "atlas_guide", map[string]any{"topic": "class:banana"}); te.Code != "invalid_argument" {
		t.Errorf("unknown class topic: %+v", te)
	}
}

// --- when Tesseract is not there ---

func TestAnUnreachableTesseractIsARetryableErrorNotAnEmptyAnswer(t *testing.T) {
	e := setup(t, q("ATLAS-Q-001", "open"))
	e.fake.Close()

	for name, args := range map[string]map[string]any{
		"atlas_get":    {"id": "ATLAS-Q-001"},
		"atlas_list":   {"class": "question"},
		"atlas_search": {"query": "anything"},
		"atlas_audit":  {},
	} {
		te := e.fail(t, name, args)
		if te.Code != "tesseract_unavailable" || !te.Retryable || !strings.Contains(te.NextStep, "TESSERACT_URL") {
			t.Errorf("%s: %+v, want a retryable tesseract_unavailable that says what to check", name, te)
		}
	}
}

func TestAStaleRegisterIsServedWithAWarningWhenARefreshFails(t *testing.T) {
	e := setup(t, q("ATLAS-Q-001", "open"))
	if got := ids(e.call(t, "atlas_list", map[string]any{"class": "question"})); len(got) != 1 {
		t.Fatalf("first list = %v", got)
	}
	e.fake.Close()

	res := e.call(t, "atlas_list", map[string]any{"class": "question", "refresh": true})
	if got := ids(res); !slices.Equal(got, []string{"ATLAS-Q-001"}) {
		t.Errorf("stale list = %v", got)
	}
	warnings, _ := res["warnings"].([]any)
	if len(warnings) == 0 || !strings.Contains(warnings[0].(string), "could not be refreshed") {
		t.Errorf("warnings = %v; an answer from before the failure must say so", res["warnings"])
	}
}
