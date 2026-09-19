package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	gmcpserver "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/audit"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

// scanCap is how many ranked matches atlas_search reads when it has to filter
// them itself.
const scanCap = 200

var visibilityChoices = []string{"private", "portfolio", "public", "untagged"}

// --- atlas_get ---

type relationshipView struct {
	Type        string `json:"type"`
	Target      string `json:"target"`
	Note        string `json:"note,omitempty"`
	Resolved    bool   `json:"resolved"`
	TargetClass string `json:"target_class,omitempty"`
	TargetName  string `json:"target_name,omitempty"`
	// Vocabulary is where the type stands: contract, proposed, convention, or
	// "unknown" when it is outside every list.
	Vocabulary string `json:"vocabulary"`
}

type backlinkView struct {
	ID    string   `json:"id"`
	Class string   `json:"class"`
	Name  string   `json:"name,omitempty"`
	Via   []string `json:"via"`
	Types []string `json:"types,omitempty"`
}

type findingView struct {
	Rule     string `json:"rule"`
	Basis    string `json:"basis"`
	Severity string `json:"severity"`
	ID       string `json:"id,omitempty"`
	Detail   string `json:"detail"`
}

type getResult struct {
	ID         string `json:"id"`
	Class      string `json:"class"`
	Namespace  string `json:"namespace"`
	RevisionID string `json:"revision_id"`
	ItemID     string `json:"item_id"`
	Name       string `json:"name"`
	Summary    string `json:"summary"`

	StateField      string   `json:"state_field,omitempty"`
	State           string   `json:"state,omitempty"`
	ReviewStatus    string   `json:"review_status"`
	LastReviewed    string   `json:"last_reviewed,omitempty"`
	Confidence      float64  `json:"confidence"`
	ConfidenceLabel string   `json:"confidence_label,omitempty"`
	Visibility      string   `json:"visibility,omitempty"`
	Tags            []string `json:"tags"`

	Data          map[string]any     `json:"data"`
	Body          string             `json:"body"`
	Relationships []relationshipView `json:"relationships"`
	Backlinks     []backlinkView     `json:"backlinks,omitempty"`
	Findings      []findingView      `json:"findings,omitempty"`

	Kind      string   `json:"kind,omitempty"`
	Source    string   `json:"source,omitempty"`
	Pointer   string   `json:"pointer,omitempty"`
	Author    string   `json:"author,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

func (s *service) getTool() definition {
	return definition{
		name: "atlas_get",
		description: "Fetch one Atlas record by key (for example ATLAS-Q-014 or ATLAS-ASSET-027) with its data, body, state and relationships. " +
			"The class and namespace come from the key; never pass a namespace. " +
			"Relationship targets are resolved to their class and name, and a target that does not exist is marked resolved:false. " +
			"Also resolves document keys (ADRs, investigations, ATLAS-META-* records). " +
			"Returns the record as stored, including the raw review_status, even where it disagrees with the contract. " +
			"Pass include=[\"backlinks\"] for what points at this record and include=[\"findings\"] for audit findings about it. " +
			"A key that is not found comes back with near matches.",
		props: []gmcpserver.Prop{
			gmcpserver.StringProp("id", "The record key, e.g. ATLAS-INIT-002. Case and zero-padding are forgiven (atlas-q-14 finds ATLAS-Q-014).", true),
			gmcpserver.StringArrayProp("include", "Extras to compute: \"backlinks\" and/or \"findings\".", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.get,
	}
}

func (s *service) get(ctx context.Context, args map[string]any) (any, error) {
	id := contract.NormalizeID(argString(args, "id"))
	include := argStrings(args, "include")
	for _, inc := range include {
		if inc != "backlinks" && inc != "findings" {
			return nil, invalid("include", fmt.Sprintf("include %q is not one of backlinks, findings", inc))
		}
	}

	var rec *corpus.Record
	if class, _, ok := contract.ParseID(id); ok {
		rev, err := s.Client.GetCurrent(ctx, class.Namespace(), id)
		switch {
		case err == nil:
			rec = corpus.FromRevision(rev)
		case !errors.Is(err, tesseract.ErrNotFound):
			return nil, upstream(err)
		}
	}

	// The corpus resolves relationship targets, backlinks and findings, and is
	// where a document key is found. If it cannot load, a record already in
	// hand is still worth returning.
	c, corpusErr := s.corpus(ctx, args)
	if rec == nil {
		if corpusErr != nil {
			return nil, corpusErr
		}
		found, ok := c.ByKey[id]
		if !ok {
			return nil, s.notFoundKey(id, c)
		}
		rec = found
	}

	out := &getResult{
		ID: rec.Key, Class: rec.Class, Namespace: rec.Namespace, RevisionID: rec.RevisionID, ItemID: rec.ItemID,
		Name: rec.Name, Summary: rec.Summary,
		StateField: rec.StateField, State: rec.State, ReviewStatus: rec.ReviewStatus, LastReviewed: rec.LastReviewed,
		Confidence: rec.Confidence, ConfidenceLabel: rec.ConfidenceLabel, Visibility: rec.Visibility(), Tags: rec.Tags,
		Data: rec.Data, Body: rec.Body, Kind: rec.Kind, Source: rec.Source, Author: rec.Author, SessionID: rec.SessionID,
		CreatedAt: rec.CreatedAt,
	}
	if rec.PointerScheme != "" {
		out.Pointer = rec.PointerScheme + ":" + rec.PointerLocator
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Data == nil {
		out.Data = map[string]any{}
	}
	out.Relationships = relationshipViews(rec, c)

	if corpusErr != nil {
		out.Warnings = append(out.Warnings, "The register could not be loaded, so relationship targets are unresolved: "+corpusErr.Error())
	} else {
		out.Warnings = append(out.Warnings, corpusWarnings(c)...)
		if slices.Contains(include, "backlinks") {
			out.Backlinks = backlinkViews(c, rec.Key, "", "")
		}
		if slices.Contains(include, "findings") {
			for _, f := range audit.Run(c) {
				if f.Key == rec.Key {
					out.Findings = append(out.Findings, findingView{Rule: f.Rule, Basis: f.Basis, Severity: f.Severity, Detail: f.Detail})
				}
			}
		}
	}
	return out, nil
}

func relationshipViews(rec *corpus.Record, c *corpus.Corpus) []relationshipView {
	out := make([]relationshipView, 0, len(rec.Relationships))
	for _, rel := range rec.Relationships {
		v := relationshipView{Type: rel.Type, Target: rel.Target, Note: rel.Note, Vocabulary: "unknown"}
		if term, ok := contract.LookupRelationship(rel.Type); ok {
			v.Vocabulary = term.Status
		}
		if c != nil {
			if t, ok := c.Resolve(rel.Target); ok {
				v.Resolved, v.TargetClass = true, t.Class
				if t.Name != t.Key {
					v.TargetName = t.Name
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *service) notFoundKey(id string, c *corpus.Corpus) error {
	msg := fmt.Sprintf("no record has the key %q", id)
	var next []string
	if class, _, ok := contract.ParseID(id); ok {
		recs := c.OfClass(class.Key)
		if len(recs) > 0 {
			msg += fmt.Sprintf("; %s holds %d records, %s through %s", class.Namespace(), len(recs), recs[0].Key, recs[len(recs)-1].Key)
		}
		next = append(next, fmt.Sprintf("atlas_list class=%s shows the keys that exist", class.Key))
	}
	if near := c.Suggest(id, 5); len(near) > 0 {
		next = append(next, "near matches: "+strings.Join(near, ", ")+" (fetch one with atlas_get)")
	}
	return notFound(msg, strings.Join(next, ". "))
}

// --- atlas_search ---

type searchRow struct {
	row
	Score *float64 `json:"score,omitempty"`
}

func (s *service) searchTool() definition {
	return definition{
		name: "atlas_search",
		description: "Semantic and keyword search across the Atlas register and its documents (Tesseract hybrid recall, scoped to project/atlas/knowledge). " +
			"Returns compact rows (key, class, summary, state, review_status, score); use atlas_get for a whole record. " +
			"Filter by class, system tag, visibility, state (the class's own lifecycle value, such as open or extracted) or review_status. " +
			"Filters other than class are applied to the top 200 ranked matches, and the response says when that cap could be hiding more. " +
			"Set include_captures=true to also search the Atlas session captures (decisions, follow-ups and limitations written under project/atlas/memory); those are not register records. " +
			"To enumerate rather than search, use atlas_list, which is exhaustive.",
		props: []gmcpserver.Prop{
			gmcpserver.StringProp("query", "What to look for, in your own words or as an exact key or identifier.", true),
			gmcpserver.StringEnumProp("class", "Restrict to one class, or to document for records outside the seven classes.", false, classEnum(true)...),
			gmcpserver.StringProp("system", "Restrict to records tagged with this system tag (exact spelling; see atlas_tags).", false),
			gmcpserver.StringEnumProp("visibility", "Restrict by visibility tag; untagged matches records with none.", false, visibilityChoices...),
			gmcpserver.StringProp("state", "Restrict to records whose class lifecycle value equals this (status, maturity or capability_maturity, depending on class).", false),
			gmcpserver.StringProp("review_status", "Restrict to records with this raw review_status (draft or reviewed).", false),
			gmcpserver.BooleanProp("include_captures", "Also search Atlas session captures under project/atlas/memory. Cannot be combined with class.", false),
			gmcpserver.IntegerProp("limit", "Rows to return, 1 to 50. Default 10.", false),
			gmcpserver.StringProp("cursor", "next_cursor from a previous unfiltered search, to continue it.", false),
		},
		run: s.search,
	}
}

func (s *service) search(ctx context.Context, args map[string]any) (any, error) {
	query := argString(args, "query")
	class, err := classArg(args, "class", true, false)
	if err != nil {
		return nil, err
	}
	system, state, review := argString(args, "system"), argString(args, "state"), argString(args, "review_status")
	visibility := argString(args, "visibility")
	if visibility != "" && !slices.Contains(visibilityChoices, visibility) {
		return nil, invalid("visibility", fmt.Sprintf("visibility %q is not one of %s", visibility, strings.Join(visibilityChoices, ", ")))
	}
	limit := argInt(args, "limit", 10, 1, 50)
	cursor := argString(args, "cursor")
	captures := argBool(args, "include_captures")
	if captures && class != "" {
		return nil, invalid("include_captures", "include_captures cannot be combined with class: captures are not register records")
	}

	namespaces := []string{contract.Root, contract.Root + "/*"}
	if c, ok := contract.ClassByKey(class); ok {
		namespaces = []string{c.Namespace()}
	}
	if captures {
		namespaces = append(namespaces, "project/atlas/memory/*")
	}

	filtering := class == corpus.DocumentClass || system != "" || visibility != "" || state != "" || review != ""
	if filtering && cursor != "" {
		return nil, invalid("cursor", "cursor cannot continue a filtered search: filters are applied to the top ranked matches, so there is no cursor to follow. Narrow the query, or use atlas_list.")
	}
	req := tesseract.RecallRequest{Namespaces: namespaces, Query: query, Ranking: "relevance", PayloadMode: "summary", Limit: limit, Cursor: cursor}
	if filtering {
		req.Limit = scanCap
	}
	res, err := s.Client.Recall(ctx, req)
	if err != nil {
		return nil, upstream(err)
	}

	var rows []searchRow
	for _, hit := range res.Results {
		rec := corpus.FromRevision(hit.Revision)
		r := searchRow{row: rowOf(rec), Score: hit.Score}
		if hit.Revision.Domain == "memory" {
			r.Class, r.Namespace = "capture", hit.Revision.Namespace
		}
		if filtering && !matchesFilters(rec, class, system, visibility, state, review) {
			continue
		}
		if r.Score != nil {
			rounded := float64(int(*r.Score*10000+0.5)) / 10000
			r.Score = &rounded
		}
		rows = append(rows, r)
	}

	out := map[string]any{"query": query}
	if filtering {
		rows = rows[:min(len(rows), limit)]
		out["scanned"] = len(res.Results)
		if res.Manifest.ResultsTotal > len(res.Results) {
			out["note"] = fmt.Sprintf("Filters were applied to the top %d of %d ranked matches; more matching records may exist. Use atlas_list for an exhaustive filtered view.", len(res.Results), res.Manifest.ResultsTotal)
		}
	} else {
		out["total"] = res.Manifest.ResultsTotal
		if res.Manifest.NextCursor != "" {
			out["next_cursor"] = res.Manifest.NextCursor
		}
	}
	if rows == nil {
		rows = []searchRow{}
	}
	out["items"], out["count"] = rows, len(rows)
	return out, nil
}

// matchesFilters applies the filters Tesseract's recall cannot: it works on
// the summary-mode record, which carries tags and consumer_state.
func matchesFilters(r *corpus.Record, class, system, visibility, state, review string) bool {
	if class == corpus.DocumentClass && !r.IsDocument() {
		return false
	}
	return recordMatches(r, system, visibility, state, review)
}

func recordMatches(r *corpus.Record, system, visibility, state, review string) bool {
	if system != "" && !r.HasTag(system) {
		return false
	}
	if visibility != "" {
		if visibility == "untagged" {
			if r.Visibility() != "" {
				return false
			}
		} else if r.Visibility() != visibility {
			return false
		}
	}
	if state != "" && r.State != state {
		return false
	}
	return review == "" || r.ReviewStatus == review
}

// --- atlas_list ---

func (s *service) listTool() definition {
	return definition{
		name: "atlas_list",
		description: "Enumerate the records of one class completely, in key order, with filters. " +
			"Unlike a search the result is exhaustive: total is the full count matching your filters, and next_offset is null only when you have everything. " +
			"Use it to see what exists in a class, to find records in a state (state=open), or to find records still to review (review_status=draft). " +
			"class=document lists the records outside the seven classes (ADRs, investigations, META records, root documents). " +
			"Rows are compact; use atlas_get for a whole record.",
		props: []gmcpserver.Prop{
			gmcpserver.StringEnumProp("class", "The class to list, or document.", true, classEnum(true)...),
			gmcpserver.StringProp("state", "Only records whose class lifecycle value equals this (status, maturity or capability_maturity, depending on class).", false),
			gmcpserver.StringProp("review_status", "Only records with this raw review_status.", false),
			gmcpserver.StringProp("system", "Only records tagged with this system tag (exact spelling; see atlas_tags).", false),
			gmcpserver.StringEnumProp("visibility", "Only records with this visibility tag; untagged matches records with none.", false, visibilityChoices...),
			gmcpserver.StringEnumProp("sort", "key (default, natural order), newest or oldest by creation time.", false, "key", "newest", "oldest"),
			gmcpserver.IntegerProp("limit", "Rows per page, 1 to 200. Default 50.", false),
			gmcpserver.IntegerProp("offset", "Rows to skip; use next_offset from the previous page.", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.list,
	}
}

func (s *service) list(ctx context.Context, args map[string]any) (any, error) {
	class, err := classArg(args, "class", true, true)
	if err != nil {
		return nil, err
	}
	visibility, sortBy := argString(args, "visibility"), argString(args, "sort")
	if visibility != "" && !slices.Contains(visibilityChoices, visibility) {
		return nil, invalid("visibility", fmt.Sprintf("visibility %q is not one of %s", visibility, strings.Join(visibilityChoices, ", ")))
	}
	if sortBy != "" && !slices.Contains([]string{"key", "newest", "oldest"}, sortBy) {
		return nil, invalid("sort", fmt.Sprintf("sort %q is not one of key, newest, oldest", sortBy))
	}
	c, err := s.corpus(ctx, args)
	if err != nil {
		return nil, err
	}

	var recs []*corpus.Record
	for _, r := range c.OfClass(class) {
		if recordMatches(r, argString(args, "system"), visibility, argString(args, "state"), argString(args, "review_status")) {
			recs = append(recs, r)
		}
	}
	switch sortBy {
	case "newest":
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt > recs[j].CreatedAt })
	case "oldest":
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt < recs[j].CreatedAt })
	}
	rows := make([]row, len(recs))
	for i, r := range recs {
		rows[i] = rowOf(r)
	}
	hint := ""
	if len(rows) > 0 {
		hint = "atlas_get id=<id> returns a whole record."
	}
	return paged(rows, argInt(args, "offset", 0, 0, 1<<30), argInt(args, "limit", 50, 1, 200), hint, corpusWarnings(c)), nil
}

// --- atlas_backlinks ---

func backlinkViews(c *corpus.Corpus, key, typeFilter, classFilter string) []backlinkView {
	byFrom := map[string]*backlinkView{}
	var order []string
	for _, l := range c.Inbound(key) {
		if classFilter != "" && l.From.Class != classFilter {
			continue
		}
		if typeFilter != "" && !(l.Via == "relationship" && l.Type == typeFilter) {
			continue
		}
		v, ok := byFrom[l.From.Key]
		if !ok {
			v = &backlinkView{ID: l.From.Key, Class: l.From.Class}
			if l.From.Name != l.From.Key {
				v.Name = l.From.Name
			}
			byFrom[l.From.Key] = v
			order = append(order, l.From.Key)
		}
		if !slices.Contains(v.Via, l.Via) {
			v.Via = append(v.Via, l.Via)
		}
		if l.Via == "relationship" && !slices.Contains(v.Types, l.Type) {
			v.Types = append(v.Types, l.Type)
		}
	}
	out := make([]backlinkView, 0, len(order))
	for _, k := range order {
		out = append(out, *byFrom[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if classRank(out[i].Class) != classRank(out[j].Class) {
			return classRank(out[i].Class) < classRank(out[j].Class)
		}
		return corpus.KeyLess(out[i].ID, out[j].ID)
	})
	return out
}

func (s *service) backlinksTool() definition {
	return definition{
		name: "atlas_backlinks",
		description: "List the records that point at a key, through data.relationships (with the relationship type) and through [[wikilinks]] in bodies. " +
			"Answers \"what depends on X\" across every class, which Tesseract cannot: its link graph only sees plain wikilinks, and data.relationships is never indexed. " +
			"Works for a key that has no record, which is how to find who cites a record that was never written. " +
			"Filter by relationship type or by the class of the referring record.",
		props: []gmcpserver.Prop{
			gmcpserver.StringProp("id", "The key to find references to, e.g. ATLAS-INIT-014. It does not have to exist.", true),
			gmcpserver.StringProp("type", "Only references made with this relationship type (e.g. depends_on).", false),
			gmcpserver.StringEnumProp("class", "Only references from records of this class.", false, classEnum(true)...),
			gmcpserver.IntegerProp("limit", "Rows per page, 1 to 200. Default 50.", false),
			gmcpserver.IntegerProp("offset", "Rows to skip; use next_offset from the previous page.", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.backlinks,
	}
}

func (s *service) backlinks(ctx context.Context, args map[string]any) (any, error) {
	id := corpus.NormalizeTarget(argString(args, "id"))
	class, err := classArg(args, "class", true, false)
	if err != nil {
		return nil, err
	}
	c, err := s.corpus(ctx, args)
	if err != nil {
		return nil, err
	}
	rec, exists := c.ByKey[id]
	views := backlinkViews(c, id, argString(args, "type"), class)
	res := paged(views, argInt(args, "offset", 0, 0, 1<<30), argInt(args, "limit", 50, 1, 200), "", corpusWarnings(c))
	out := map[string]any{"id": id, "exists": exists}
	if exists {
		out["class"], out["name"] = rec.Class, rec.Name
	} else {
		out["note"] = "No record has this key. The references below cite it anyway, so they dangle."
	}
	out["items"], out["count"], out["total"], out["offset"], out["next_offset"], out["truncated"] =
		res.Items, res.Count, res.Total, res.Offset, res.NextOffset, res.Truncated
	if len(res.Warnings) > 0 {
		out["warnings"] = res.Warnings
	}
	return out, nil
}

// --- atlas_review_queue ---

type queueItem struct {
	ID           string `json:"id"`
	Class        string `json:"class"`
	Name         string `json:"name,omitempty"`
	State        string `json:"state,omitempty"`
	ReviewStatus string `json:"review_status"`
	CreatedAt    string `json:"created_at,omitempty"`
	AgeDays      int    `json:"age_days,omitempty"`
}

func (s *service) reviewQueueTool() definition {
	return definition{
		name: "atlas_review_queue",
		description: "What still needs review: records whose review_status is not reviewed, oldest first, with counts by class and by the raw review_status values present " +
			"(a value outside draft/reviewed shows up in that count). " +
			"Read-only. A review is Chrispian's word, never the author's, and this server has no way to mark a record reviewed.",
		props: []gmcpserver.Prop{
			gmcpserver.StringEnumProp("class", "Only this class.", false, classEnum(false)...),
			gmcpserver.IntegerProp("limit", "Rows per page, 1 to 200. Default 25.", false),
			gmcpserver.IntegerProp("offset", "Rows to skip; use next_offset from the previous page.", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.reviewQueue,
	}
}

func (s *service) reviewQueue(ctx context.Context, args map[string]any) (any, error) {
	class, err := classArg(args, "class", false, false)
	if err != nil {
		return nil, err
	}
	c, err := s.corpus(ctx, args)
	if err != nil {
		return nil, err
	}

	type tally struct {
		Unreviewed int `json:"unreviewed"`
		Total      int `json:"total"`
	}
	byClass := map[string]*tally{}
	byStatus := map[string]int{}
	var queue []queueItem
	now := s.Now()
	for _, r := range c.ClassRecords() {
		status := r.ReviewStatus
		if !r.HasReviewStatus {
			status = "(absent)"
		}
		byStatus[status]++
		t := byClass[r.Class]
		if t == nil {
			t = &tally{}
			byClass[r.Class] = t
		}
		t.Total++
		if r.ReviewStatus == "reviewed" {
			continue
		}
		t.Unreviewed++
		if class != "" && r.Class != class {
			continue
		}
		item := queueItem{ID: r.Key, Class: r.Class, State: r.State, ReviewStatus: status, CreatedAt: r.CreatedAt}
		if r.Name != r.Key {
			item.Name = r.Name
		}
		if created, err := parseTime(r.CreatedAt); err == nil {
			item.AgeDays = int(now.Sub(created).Hours() / 24)
		}
		queue = append(queue, item)
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].CreatedAt < queue[j].CreatedAt })

	res := paged(queue, argInt(args, "offset", 0, 0, 1<<30), argInt(args, "limit", 25, 1, 200), "", corpusWarnings(c))
	return withWarnings(map[string]any{
		"by_class":         byClass,
		"by_review_status": byStatus,
		"items":            res.Items, "count": res.Count, "total": res.Total, "offset": res.Offset,
		"next_offset": res.NextOffset, "truncated": res.Truncated,
	}, res.Warnings), nil
}
