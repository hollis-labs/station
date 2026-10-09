package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	gmcpserver "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/audit"
	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/corpus"
)

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

var (
	basisChoices    = []string{audit.BasisContract, audit.BasisConvention, audit.BasisAmbiguity, audit.BasisObserved}
	severityChoices = []string{audit.SeverityError, audit.SeverityWarn, audit.SeverityInfo}
)

func ruleIDs() []string {
	ids := make([]string, len(audit.Catalog))
	for i, r := range audit.Catalog {
		ids[i] = r.ID
	}
	return ids
}

// --- atlas_audit ---

func (s *service) auditTool() definition {
	return definition{
		name: "atlas_audit",
		description: "Check the register against the contract and report where they disagree. " +
			"With no rule or id it returns a summary: findings per rule, how they split by basis and severity, and a few examples of each. " +
			"Pass rule (the ids are in atlas_guide topic=audit) or id to list findings one by one. " +
			"Every finding has a basis: contract (the ATLAS-META records say otherwise), convention (in use across the corpus or agreed but not yet in the contract), " +
			"ambiguity (the contract is silent or contradicts itself) or observed (drift the contract does not speak to, such as a link to a record that was never written). " +
			"It never changes anything. Whether the record or the contract is wrong is Chrispian's call, so report drift instead of fixing it in passing.",
		props: []gmcpserver.Prop{
			gmcpserver.StringEnumProp("rule", "List the findings of one rule.", false, ruleIDs()...),
			gmcpserver.StringProp("id", "List the findings about one record key.", false),
			gmcpserver.StringEnumProp("basis", "Only findings with this basis.", false, basisChoices...),
			gmcpserver.StringEnumProp("severity", "Only findings with this severity.", false, severityChoices...),
			gmcpserver.StringEnumProp("class", "Only findings about records of this class (document for records outside the seven).", false, classEnum(true)...),
			gmcpserver.IntegerProp("limit", "Findings per page when listing, 1 to 200. Default 50.", false),
			gmcpserver.IntegerProp("offset", "Findings to skip when listing; use next_offset from the previous page.", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.audit,
	}
}

type ruleSummary struct {
	Rule     string         `json:"rule"`
	Title    string         `json:"title"`
	Severity string         `json:"severity"`
	Count    int            `json:"count"`
	ByBasis  map[string]int `json:"by_basis"`
	Samples  []findingView  `json:"samples"`
}

func (s *service) audit(ctx context.Context, args map[string]any) (any, error) {
	rule, basis, severity := argString(args, "rule"), argString(args, "basis"), argString(args, "severity")
	if rule != "" {
		if _, ok := audit.RuleByID(rule); !ok {
			return nil, invalid("rule", fmt.Sprintf("unknown rule %q; the rules are: %s", rule, strings.Join(ruleIDs(), ", ")))
		}
	}
	if basis != "" && !slices.Contains(basisChoices, basis) {
		return nil, invalid("basis", fmt.Sprintf("basis %q is not one of %s", basis, strings.Join(basisChoices, ", ")))
	}
	if severity != "" && !slices.Contains(severityChoices, severity) {
		return nil, invalid("severity", fmt.Sprintf("severity %q is not one of %s", severity, strings.Join(severityChoices, ", ")))
	}
	class, err := classArg(args, "class", true, false)
	if err != nil {
		return nil, err
	}
	id := contract.NormalizeID(argString(args, "id"))

	c, err := s.corpus(ctx, args)
	if err != nil {
		return nil, err
	}
	var findings []audit.Finding
	for _, f := range audit.Run(c) {
		switch {
		case rule != "" && f.Rule != rule,
			basis != "" && f.Basis != basis,
			severity != "" && f.Severity != severity,
			class != "" && f.Class != class,
			id != "" && f.Key != id:
			continue
		}
		findings = append(findings, f)
	}

	if rule != "" || id != "" {
		views := make([]findingView, len(findings))
		for i, f := range findings {
			views[i] = findingView{Rule: f.Rule, Basis: f.Basis, Severity: f.Severity, ID: f.Key, Detail: f.Detail}
		}
		return paged(views, argInt(args, "offset", 0, 0, 1<<30), argInt(args, "limit", 50, 1, 200), "", corpusWarnings(c)), nil
	}
	return summarize(c, findings, basis == "" && severity == "" && class == "", corpusWarnings(c)), nil
}

func summarize(c *corpus.Corpus, findings []audit.Finding, unfiltered bool, warnings []string) map[string]any {
	byRule := map[string]*ruleSummary{}
	byBasis, bySeverity, byClass := map[string]int{}, map[string]int{}, map[string]int{}
	for _, f := range findings {
		rs := byRule[f.Rule]
		if rs == nil {
			def, _ := audit.RuleByID(f.Rule)
			rs = &ruleSummary{Rule: f.Rule, Title: def.Title, Severity: def.Severity, ByBasis: map[string]int{}, Samples: []findingView{}}
			byRule[f.Rule] = rs
		}
		rs.Count++
		rs.ByBasis[f.Basis]++
		if len(rs.Samples) < 2 {
			rs.Samples = append(rs.Samples, findingView{Rule: f.Rule, Basis: f.Basis, Severity: f.Severity, ID: f.Key, Detail: f.Detail})
		}
		byBasis[f.Basis]++
		bySeverity[f.Severity]++
		if f.Class != "" {
			byClass[f.Class]++
		}
	}
	var rules []*ruleSummary
	var clean []string
	for _, def := range audit.Catalog {
		if rs, ok := byRule[def.ID]; ok {
			rules = append(rules, rs)
		} else {
			clean = append(clean, def.ID)
		}
	}
	out := map[string]any{
		"records": len(c.Records), "findings": len(findings), "rules_checked": len(audit.Catalog),
		"by_basis": byBasis, "by_severity": bySeverity, "by_class": byClass, "rules": rules,
		"hint": "Pass rule=<id> to list every finding of one rule, or id=<key> for one record. Findings are evidence, not verdicts: the contract is a draft.",
	}
	if unfiltered {
		out["clean_rules"] = clean
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out
}

// --- atlas_tags ---

func (s *service) tagsTool() definition {
	return definition{
		name: "atlas_tags",
		description: "The tag vocabulary in use across the register, with a count and a kind for each tag " +
			"(contract: atlas, class and visibility tags; system: one per participating system; convention: maturity:, asset-kind:, scale:, lang:, which the contract does not define; other), " +
			"and the groups of near-duplicate spellings (agentkit and agent-kit are both in use, so a filter on one misses the other). " +
			"Pass near=<tag> before using a system tag to see whether it already exists or is spelled differently.",
		props: []gmcpserver.Prop{
			gmcpserver.StringProp("near", "A tag you mean to use; returns whether it exists and the closest existing system tags.", false),
			gmcpserver.StringProp("prefix", "Only tags starting with this.", false),
			gmcpserver.StringEnumProp("kind", "Only tags of this kind.", false, "contract", "system", "convention", "other"),
			gmcpserver.IntegerProp("min_count", "Only tags carried by at least this many records.", false),
			gmcpserver.IntegerProp("limit", "Tags per page, 1 to 500. Default 100.", false),
			gmcpserver.IntegerProp("offset", "Tags to skip; use next_offset from the previous page.", false),
			gmcpserver.BooleanProp("refresh", "Re-read the register from Tesseract instead of using the short-lived cache.", false),
		},
		run: s.tags,
	}
}

type tagRow struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
	Kind  string `json:"kind"`
}

func tagKind(tag string) string {
	switch contract.TagKind(tag) {
	case contract.TagAtlas, contract.TagClass, contract.TagVisibility, contract.TagMeta:
		return "contract"
	case contract.TagConvention:
		return "convention"
	case contract.TagSystem:
		return "system"
	}
	return "other"
}

func (s *service) tags(ctx context.Context, args map[string]any) (any, error) {
	kind := argString(args, "kind")
	if kind != "" && !slices.Contains([]string{"contract", "system", "convention", "other"}, kind) {
		return nil, invalid("kind", fmt.Sprintf("kind %q is not one of contract, system, convention, other", kind))
	}
	c, err := s.corpus(ctx, args)
	if err != nil {
		return nil, err
	}
	counts := c.TagCounts()

	if near := argString(args, "near"); near != "" {
		return nearTags(near, counts, corpusWarnings(c)), nil
	}

	prefix, minCount := argString(args, "prefix"), argInt(args, "min_count", 0, 0, 1<<30)
	var rows []tagRow
	for t, n := range counts {
		if (prefix != "" && !strings.HasPrefix(t, prefix)) || n < minCount || (kind != "" && tagKind(t) != kind) {
			continue
		}
		rows = append(rows, tagRow{Tag: t, Count: n, Kind: tagKind(t)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Tag < rows[j].Tag
	})
	res := paged(rows, argInt(args, "offset", 0, 0, 1<<30), argInt(args, "limit", 100, 1, 500), "", corpusWarnings(c))

	var dups [][]tagRow
	for _, group := range corpus.NearDuplicateTags(counts) {
		g := make([]tagRow, len(group))
		for i, t := range group {
			g[i] = tagRow{Tag: t, Count: counts[t], Kind: "system"}
		}
		dups = append(dups, g)
	}
	return withWarnings(map[string]any{
		"distinct": len(counts), "items": res.Items, "count": res.Count, "total": res.Total, "offset": res.Offset,
		"next_offset": res.NextOffset, "truncated": res.Truncated,
		"near_duplicates": dups,
	}, res.Warnings), nil
}

func nearTags(tag string, counts map[string]int, warnings []string) map[string]any {
	type match struct {
		Tag      string `json:"tag"`
		Count    int    `json:"count"`
		Distance int    `json:"distance"`
	}
	var matches []match
	squashed := corpus.TagSquash(tag)
	for t, n := range counts {
		if t == tag || contract.TagKind(t) != contract.TagSystem {
			continue
		}
		d := levenshtein(tag, t)
		if corpus.TagSquash(t) == squashed || d <= 2 {
			matches = append(matches, match{t, n, d})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Distance != matches[j].Distance {
			return matches[i].Distance < matches[j].Distance
		}
		return matches[i].Tag < matches[j].Tag
	})
	advice := "This tag is not in use and nothing close to it is; a new system tag is fine if it names a participating system."
	switch {
	case counts[tag] > 0 && len(matches) == 0:
		advice = "This tag exists and has no near-duplicate; use it as spelled."
	case counts[tag] > 0:
		advice = "This tag exists, but similar spellings do too; check the matches before choosing."
	case len(matches) > 0:
		advice = "This exact tag is not in use, but similar ones are. Use the existing spelling unless it names something different."
	}
	if matches == nil {
		matches = []match{}
	}
	out := map[string]any{"tag": tag, "exists": counts[tag] > 0, "count": counts[tag], "near": matches, "advice": advice}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out
}

// levenshtein is the edit distance between two short tags.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
