package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	gmcpserver "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"

	"github.com/hollis-labs/station/internal/atlas/audit"
	"github.com/hollis-labs/station/internal/atlas/contract"
)

var guideTopics = []string{"classes", "class:<name>", "vocabularies", "tags", "lifecycle", "boundaries", "audit", "tools"}

func (s *service) guideTool() definition {
	return definition{
		name: "atlas_guide",
		description: "Read this first. The Atlas operating guide: what the Atlas is, how to choose a record class, the key scheme, " +
			"the closed vocabularies (relationship types, lifecycle states, visibility), the review and promotion lifecycle, " +
			"and what does NOT belong in the Atlas (session captures, Torque work, project ADRs, handoffs). " +
			"Call with no topic for the overview and the list of topics; pass topic for one section: " + strings.Join(guideTopics, ", ") + ". Returns markdown.",
		props: []gmcpserver.Prop{
			gmcpserver.StringProp("topic", "One of: "+strings.Join(guideTopics, ", ")+". Omit for the overview.", false),
		},
		run: s.guide,
	}
}

func (s *service) guide(_ context.Context, args map[string]any) (any, error) {
	topic := strings.ToLower(argString(args, "topic"))
	switch {
	case topic == "":
		return overviewGuide(), nil
	case topic == "classes":
		return classesGuide(), nil
	case strings.HasPrefix(topic, "class:"):
		name := strings.TrimPrefix(topic, "class:")
		c, ok := contract.ClassByName(name)
		if !ok {
			return nil, invalid("topic", fmt.Sprintf("unknown class %q in topic; use class:<one of %s>", name, strings.Join(contract.ClassNames(), ", ")))
		}
		return classGuide(c), nil
	case topic == "vocabularies":
		return vocabulariesGuide(), nil
	case topic == "tags":
		return tagsGuide(), nil
	case topic == "lifecycle":
		return lifecycleGuide(), nil
	case topic == "boundaries":
		return boundariesGuide(), nil
	case topic == "audit":
		return auditGuide(), nil
	case topic == "tools":
		return s.toolsGuide(), nil
	}
	return nil, invalid("topic", fmt.Sprintf("unknown topic %q; the topics are: %s", topic, strings.Join(guideTopics, ", ")))
}

// table renders a markdown table.
func table(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(headers, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat("---|", len(headers)) + "\n")
	for _, r := range rows {
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
	return b.String()
}

func overviewGuide() string {
	var rows [][]string
	for _, c := range contract.Classes {
		rows = append(rows, []string{c.Name, "`" + c.Prefix + "NNN`", c.Chooser, c.NotThis})
	}
	return `# The Atlas

The Portfolio Atlas is Chrispian's curated, cross-project record layer: assets, initiatives, decisions, working notes, use cases, questions and follow-ups. It describes projects without governing them. A repository or service stays authoritative for its own implementation and operating facts, and Torque for executable work. When an Atlas record and its source disagree, the source is true and the record is stale.

Records are Tesseract knowledge under ` + "`" + contract.Root + "`" + `, one namespace per class. These tools are scoped to that namespace: you never pass one, and a key names its class (ATLAS-Q-014 is a Question).

The contract that defines the records is the ATLAS-META-* records in ` + "`" + contract.Root + `/meta` + "`" + `. It is a draft under Chrispian's review, so a record can disagree with it. Read the disagreement as information and do not fix it in passing (topic: audit).

## Reading

This server is read-only for now.

- ` + "`atlas_get`" + ` one record by key, with relationship targets resolved.
- ` + "`atlas_search`" + ` find records by meaning or keyword.
- ` + "`atlas_list`" + ` every record of a class, with filters. Exhaustive.
- ` + "`atlas_backlinks`" + ` what points at a key, across all classes.
- ` + "`atlas_review_queue`" + ` what is still unreviewed.
- ` + "`atlas_audit`" + ` where records and the contract disagree.
- ` + "`atlas_tags`" + ` the tag vocabulary and near-duplicate spellings.

## Writing

There are no write tools yet. The Atlas is curated, not agent memory: write a record when Chrispian asks for one, it enters as ` + "`review_status: draft`" + `, and only he marks it reviewed. Until write tools exist, follow the atlas-record skill (a knowledge_write into the class namespace) where it is installed.

## Which class?

` + table([]string{"Class", "Key", "It is", "Not this"}, rows) + `
The class cannot be changed afterwards: Tesseract does not reclassify a record. A note that firms up becomes a new record that cites the old one.

## Topics

Call again with topic=` + strings.Join(guideTopics, ", ") + `.
`
}

func classesGuide() string {
	var rows [][]string
	for _, c := range contract.Classes {
		state, vocab := "none", "-"
		if c.StateField != "" {
			state, vocab = "consumer_state."+c.StateField, strings.Join(c.States, " / ")
		}
		rows = append(rows, []string{c.Name, "`" + c.Prefix + "NNN`", "`" + c.Segment + "`", "`" + c.Tag() + "`", state, vocab})
	}
	return "# Classes\n\nKeys are the prefix plus at least three zero-padded digits, sequential per class. A key is also the exact string a `[[wikilink]]` must name to resolve. " +
		"Every class also carries `consumer_state.review_status` (draft or reviewed) and `last_reviewed`.\n\n" +
		table([]string{"Class", "Key", "Namespace under " + contract.Root, "Class tag", "Lifecycle field", "Values"}, rows) +
		"\nThe lifecycle vocabularies differ on purpose: they track different things. `status: active` is an Initiative word and means nothing on a Decision. Use `class:<name>` for one class's data shape.\n"
}

var classNotes = map[string]string{
	"initiative": "An outcome being advanced across projects, not a task list: it records movement and direction and links to Torque. A qualifier after a slash in a status line (`active / foundation phase`) is `phase`, not part of `status`. `participating_systems` is also flattened into tags, because `data` is never filtered on.",
	"asset":      "Described in four layers: local mechanism, immediate composition, portfolio role, operator intent. Omit `canonical_location` rather than guess it. Per-consumer adoption belongs on the relationship (`consumes` with an `adoption_state`), never as one field on the asset. Capability assets (`asset_kind: capability`) are a convention in use, not in the contract: one `implemented_within` relationship to a parent Asset and a wikilink to it in the body.",
	"decision":   "A cross-project chosen direction and its rationale. A project's own ADR stays in that project. A superseded or reversed Decision is neither edited in place nor deleted: write the successor as its own record with a `supersedes` relationship to the old key, and revise the old one to `status: superseded`, so the two say the same thing.",
	"note":       "Current thinking, deliberately provisional, so it has no lifecycle field: a maturity value would overstate it. When it firms up it becomes a Decision, Initiative or Question, as a new record that cites the note.",
	"use-case":   "A concrete way assets compose. `flow` is the participating assets in composition order (`Asset A -> Asset B`).",
	"question":   "Worth a record only if it says what would resolve it. `moot` (the situation changed) is not `answered` (it got an answer). When answered, add a relationship to the Decision or record that answered it rather than editing the answer into the question.",
	"followup":   "A candidate todo that stays here until it is explicitly moved to Torque or another authority. `promoted_to` stays empty until promotion, then carries the Torque task id. `declined` is kept, not deleted, so \"we considered this and chose not to\" survives.",
}

func classGuide(c contract.Class) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (`%sNNN`)\n\nNamespace `%s`. Class tag `%s`.\n\nUse it for: %s. Not: %s.\n\n", c.Name, c.Prefix, c.Namespace(), c.Tag(), c.Chooser, c.NotThis)
	if c.StateField != "" {
		fmt.Fprintf(&b, "Lifecycle: `consumer_state.%s`, one of %s.\n\n", c.StateField, strings.Join(c.States, " / "))
	} else {
		b.WriteString("Lifecycle: none beyond `review_status`.\n\n")
	}
	var keys []string
	for _, k := range c.DataKeys {
		if slices.Contains(c.Required, k) {
			keys = append(keys, "`"+k+"`")
		} else {
			keys = append(keys, "`"+k+"` (optional)")
		}
	}
	fmt.Fprintf(&b, "`data` keys: %s.\n", strings.Join(keys, ", "))
	if len(c.Conventions) > 0 {
		fmt.Fprintf(&b, "In use across the corpus but not in the template (convention): %s.\n", "`"+strings.Join(c.Conventions, "`, `")+"`")
	}
	if note := classNotes[c.Key]; note != "" {
		b.WriteString("\n" + note + "\n")
	}
	b.WriteString("\n" + `Shared by every class: ` + "`kind: note`" + `; tags ` + "`atlas`, `" + c.Tag() + "`" + `, one ` + "`visibility:`" + ` tag and one tag per participating system; ` +
		"`data.relationships` as `{type, target, note?}`, with each Atlas target also written as a `[[wikilink]]` in the body (the array is display only and never joins Tesseract's link graph); " +
		"body prose for the fields worth finding, because `data` is never searched.\n")
	return b.String()
}

func vocabulariesGuide() string {
	var rels [][]string
	for _, t := range contract.RelationshipTypes {
		note := t.Note
		if t.Alias != "" && note == "" {
			note = "stands in for " + t.Alias
		}
		rels = append(rels, []string{"`" + t.Name + "`", t.Status, note})
	}
	var conf [][]string
	for _, c := range contract.ConfidenceLabels {
		conf = append(conf, []string{c.Label, fmt.Sprint(c.Value)})
	}
	return "# Vocabularies\n\n## Relationship types (`data.relationships[].type`)\n\n" +
		"Status says where a term stands: **contract** is in the ATLAS-META records; **proposed** was agreed on 2026-09-19 and is not written into them yet; **convention** is in use and ratified by no one. " +
		"`relates_to` is the honest fallback: a forced specific verb is worse than it. A new term is proposed to Chrispian, not coined because one record used it.\n\n" +
		table([]string{"Type", "Status", "Note"}, rels) +
		"\n`target` is another Atlas key, or an external reference (a project slug, a Torque id). The agreed direction is typed prefixes (`atlas:`, `project:`, `torque:`, `file:`); today most external targets are bare slugs.\n\n" +
		"## Visibility (tag `visibility:<value>`)\n\n" + strings.Join(contract.Visibilities, ", ") + ". The contract's default is `" + contract.DefaultVisibility +
		"`. It is a label, not access control: Tesseract fences nothing under `project/*`.\n\n" +
		"## Asset kinds (`data.asset_kind`)\n\n" + strings.Join(contract.AssetKinds, ", ") + ".\n\n" +
		"## Review status (`consumer_state.review_status`)\n\n" + strings.Join(contract.ReviewStatuses, ", ") + ".\n\n" +
		"## Confidence\n\nThe top-level numeric `confidence` is a ranking input, mapped from a qualitative label:\n\n" + table([]string{"Label", "Value"}, conf)
}

func tagsGuide() string {
	return "# Tags\n\nEvery class record carries three independent dimensions:\n\n" +
		"1. **Class:** `atlas` and `atlas:<class>`.\n" +
		"2. **Systems:** one lowercase-hyphenated tag per participating system (`go-agent-wrapper`, not `go-agent-wrapper (acp)`). Skip a generic phrase that is not a system's name.\n" +
		"3. **Visibility:** exactly one `visibility:` tag. `portfolio` unless Chrispian says otherwise.\n\n" +
		"Confidence, maturity and status are not tags: a changed tag has no history, while a changed `consumer_state` is a revision. " +
		"The corpus nonetheless carries `maturity:`, `asset-kind:`, `scale:` and `lang:` tags. That is a convention no one has ratified, not part of the contract; do not add a tag dimension of your own.\n\n" +
		"Spelling matters because a filter on one spelling misses the other, and `agentkit` and `agent-kit` both exist. Before using a system tag, call `atlas_tags` with `near=<tag>`.\n"
}

func lifecycleGuide() string {
	return `# Lifecycle

Two different events both read as "done", and the contract renamed one of them after they had been confused.

**Reviewed** asks whether a record has been checked and fairly represents what it describes: ` + "`consumer_state.review_status`" + ` moves draft to reviewed. It is Chrispian's word, or an agent's under his direction, never the author flipping its own writing. ` + "`last_reviewed`" + ` is the date of the review, not of the write.

**Promoted** asks whether a record has left the curated layer to become an authority somewhere else. Only some classes have a promotion, and it is never automatic:

- A Follow-up becomes a Torque task: ` + "`status: promoted`" + `, ` + "`data.promoted_to`" + ` the task id.
- A Decision may produce a project ADR: it stays current with a ` + "`produces`" + ` relationship to it. The Atlas record is the cross-project view and the ADR is project-local authority, so neither supersedes the other.
- Initiatives, Assets, Use cases, Notes and Questions are not promoted; they are revised. An Initiative ends at completed, superseded or cancelled, not at a move.

## How Tesseract treats the records

- Tesseract's own revision ` + "`status`" + ` reads ` + "`canonical`" + ` on every knowledge write. It is not the lifecycle, so never read or filter on it; the lifecycle lives in ` + "`consumer_state`" + `.
- A write to an existing key adds a revision to that entry and becomes its head. There is no partial update: a revision resupplies every field.
- ` + "`supersedes`" + ` on a write is revision lineage inside one entry. It cannot link two records, so a replaced Decision is marked by a ` + "`supersedes`" + ` relationship from its successor, not by that parameter.
`
}

func boundariesGuide() string {
	return "# What does not belong in the Atlas\n\n" + table([]string{"It is", "It goes"}, [][]string{
		{"what a session decided, learned or deferred", "Tesseract memory under `project/atlas/memory/` (decisions, followups, limitations), written by the capture-* skills. Not register records, though `atlas_search include_captures=true` finds them. Note the name collision: an Atlas Decision or Follow-up is a different thing from those memory types."},
		{"a project's own decision record", "that project's `knowledge/adr`; an Atlas Decision may cite it"},
		{"tracked, executable work", "Torque. A Follow-up is only a candidate until it is moved there"},
		{"a handoff to the next session", "Tesseract workspace, `project/<project>/workspace/handoff`"},
		{"a project's implementation or operating fact", "the project's source; the Atlas links to it. When they disagree, the source is true"},
		{"a numbered draft under ~/dev/chrispian/atlas-content/drafts", "the working export and safety net while content moves into Tesseract. Moving a draft to processed/ is Chrispian's checkpoint, not a cleanup step"},
		{"a deep-dive document rather than a record", "the seven classes have no place for one, and documents already sit outside them (root, adr, investigations, meta). Ask Chrispian where it goes; do not add a pattern beside those"},
	})
}

func auditGuide() string {
	var rules [][]string
	for _, r := range audit.Catalog {
		rules = append(rules, []string{"`" + r.ID + "`", r.Basis, r.Severity, r.Title})
	}
	return `# Reading an audit

` + "`atlas_audit`" + ` measures records against the contract and reports where they disagree. It is evidence, not a verdict, because the contract is a draft and the corpus has moved faster than it. Whether the record or the contract is right is Chrispian's call; report drift instead of correcting it in passing, since a quiet fix of someone else's record removes the evidence.

Every finding has a **basis**:

- **contract**: the ATLAS-META records say otherwise, plainly.
- **convention**: in use across the corpus, or agreed on 2026-09-19, but not in the contract yet.
- **ambiguity**: the contract is silent, or disagrees with itself.
- **observed**: drift the contract does not speak to, such as a link to a record that was never written.

Severity is error, warn or info, and is the rule's default; a rule can report a lower severity for a case it is less sure of.

## What it cannot see

State that lags the world. A Follow-up can have been executed in Torque and still read draft with an empty promoted_to, and nothing in the register says so. Before treating a draft or ready Follow-up as open, look for its work in Torque.

## Rules

` + table([]string{"Rule", "Default basis", "Severity", "Checks"}, rules)
}

func (s *service) toolsGuide() string {
	var rows [][]string
	for _, d := range s.definitions() {
		first, _, _ := strings.Cut(d.description, ". ")
		rows = append(rows, []string{"`" + d.name + "`", strings.TrimSuffix(first, ".") + "."})
	}
	return "# Tools\n\nAll read-only; none takes a namespace.\n\n" + table([]string{"Tool", "What it does"}, rows)
}
