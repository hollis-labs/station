// Package corpus loads the Atlas register out of Tesseract into an in-memory
// model the read tools query: every class record and every document under
// project/atlas/knowledge, as stored.
//
// "As stored" is deliberate. Nothing here normalizes, repairs or hides a
// value that disagrees with the contract (a review_status outside
// draft/reviewed, a free-text asset_kind, a relationship verb outside the
// closed set): a reader who cannot see the disagreement cannot decide whether
// the contract or the record is wrong, and that decision is Chrispian's.
// Judging records against the contract is the audit package's job.
package corpus

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hollis-labs/station/internal/atlas/contract"
	"github.com/hollis-labs/station/internal/atlas/tesseract"
)

// DocumentClass is the Class of a record outside the seven class namespaces:
// an ADR, an investigation, a META record, or one of the root documents. It is
// not an Atlas class; the contract has no place for these records, and
// "document" is only the word the viewer and this server use for them.
const DocumentClass = "document"

// maxRecords bounds a load. The whole register is a few hundred records; a
// load that reaches this is a symptom, reported as a warning.
const maxRecords = 20000

// Relationship is one entry of a record's data.relationships.
type Relationship struct {
	Type   string
	Target string
	Note   string
	// Malformed is set for an entry that is not an object with a string
	// type and target; Target then holds whatever it was, printed.
	Malformed bool
}

// Record is one knowledge record as stored, with the fields Atlas reads
// pulled out of `data` and `consumer_state`.
type Record struct {
	Key        string
	Class      string // a contract.Class key, or DocumentClass
	Namespace  string
	RevisionID string
	ItemID     string

	Name    string
	Summary string
	Body    string
	Tags    []string

	Data     map[string]any
	Consumer map[string]any

	// ReviewStatus is consumer_state.review_status exactly as stored; the
	// contract allows draft and reviewed only, and HasReviewStatus tells an
	// absent field from an empty one.
	ReviewStatus    string
	HasReviewStatus bool
	LastReviewed    string

	// StateField and State are the class's own lifecycle field and its stored
	// value; both are empty for a class with no lifecycle field and for
	// documents.
	StateField string
	State      string

	ConfidenceLabel string
	Confidence      float64

	Relationships []Relationship
	Wikilinks     []string

	Kind           string
	Source         string
	PointerScheme  string
	PointerLocator string
	Author         string
	SessionID      string
	CreatedAt      string
}

// IsDocument reports whether the record sits outside the seven classes.
func (r *Record) IsDocument() bool { return r.Class == DocumentClass }

// ClassDef returns the contract class of a class record.
func (r *Record) ClassDef() (contract.Class, bool) { return contract.ClassByKey(r.Class) }

// Systems returns the record's system tags: the tags that name a participating
// system, as opposed to the atlas, class, visibility and convention tags.
func (r *Record) Systems() []string {
	var out []string
	for _, t := range r.Tags {
		if contract.TagKind(t) == contract.TagSystem {
			out = append(out, t)
		}
	}
	return out
}

// Visibility returns the record's visibility value, or "" when it carries no
// visibility tag. A record with several returns the first; the audit reports
// the disagreement.
func (r *Record) Visibility() string {
	if v := contract.VisibilityOf(r.Tags); len(v) > 0 {
		return v[0]
	}
	return ""
}

// HasTag reports whether the record carries tag.
func (r *Record) HasTag(tag string) bool { return slices.Contains(r.Tags, tag) }

var wikilinkPattern = regexp.MustCompile(`\[\[([^\[\]\n]+)\]\]`)

// FromRevision converts a Tesseract revision into a Record.
func FromRevision(rev tesseract.Revision) *Record {
	r := &Record{
		Key:        rev.MemoryKey,
		Class:      DocumentClass,
		Namespace:  rev.Namespace,
		RevisionID: rev.RevisionID,
		ItemID:     rev.ItemID,
		Summary:    rev.Payload.Summary,
		Body:       rev.Payload.Body,
		Tags:       rev.Tags,
		Confidence: rev.Confidence,
		Kind:       rev.Facets.Kind,
		Source:     rev.Facets.Source,
		Author:     rev.Author.AgentID,
		SessionID:  rev.SessionID,
		CreatedAt:  rev.CreatedAt,
	}
	if rev.Facets.Pointer != nil {
		r.PointerScheme, r.PointerLocator = rev.Facets.Pointer.Scheme, rev.Facets.Pointer.Locator
	}
	r.Data = object(rev.Payload.Data)
	r.Consumer = object(rev.ConsumerState)

	if c, ok := contract.ClassOfNamespace(rev.Namespace); ok {
		r.Class = c.Key
		r.StateField = c.StateField
		if c.StateField != "" {
			r.State = text(r.Consumer[c.StateField])
		}
	}

	r.Name = text(r.Data["name"])
	if r.Name == "" {
		r.Name = r.Key
	}
	if v, ok := r.Consumer["review_status"]; ok {
		r.HasReviewStatus = true
		r.ReviewStatus = text(v)
	}
	r.LastReviewed = text(r.Consumer["last_reviewed"])
	r.ConfidenceLabel = text(r.Consumer["confidence"])
	r.Relationships = relationships(r.Data["relationships"])

	seen := map[string]bool{}
	for _, m := range wikilinkPattern.FindAllStringSubmatch(r.Body, -1) {
		target, _, _ := strings.Cut(m[1], "|")
		target = strings.TrimSpace(target)
		if target != "" && !seen[target] {
			seen[target] = true
			r.Wikilinks = append(r.Wikilinks, target)
		}
	}
	return r
}

func object(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// text renders a stored scalar as a string; nil is "".
func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func relationships(v any) []Relationship {
	list, _ := v.([]any)
	out := make([]Relationship, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		typ, typOK := m["type"].(string)
		target, targetOK := m["target"].(string)
		if !ok || !typOK || !targetOK {
			out = append(out, Relationship{Target: text(item), Malformed: true})
			continue
		}
		out = append(out, Relationship{Type: typ, Target: target, Note: text(m["note"])})
	}
	return out
}

// Link is one inbound reference to a record: another record that names it in
// data.relationships, or links to it with a [[wikilink]] in its body.
type Link struct {
	From *Record
	Via  string // "relationship" or "wikilink"
	Type string // the relationship type, for Via "relationship"
	Note string
}

// Corpus is the whole register at one moment.
type Corpus struct {
	// Records are ordered by class (the contract's order, documents last) and
	// then by key, numerically within a prefix.
	Records []*Record
	ByKey   map[string]*Record
	// Duplicates holds keys stored in more than one knowledge namespace; ByKey
	// keeps the first.
	Duplicates map[string][]*Record

	LoadedAt time.Time
	// Stale is set on a corpus served after a refresh failed; StaleReason says
	// why. Warnings carry anything else a reader should know about the load.
	Stale       bool
	StaleReason string
	Warnings    []string

	inbound map[string][]Link
}

// Build indexes a set of revisions into a Corpus.
func Build(revs []tesseract.Revision, now time.Time) *Corpus {
	c := &Corpus{
		ByKey:      make(map[string]*Record, len(revs)),
		Duplicates: map[string][]*Record{},
		LoadedAt:   now,
		inbound:    map[string][]Link{},
	}
	seen := map[string]bool{}
	for _, rev := range revs {
		if rev.RevisionID != "" && seen[rev.RevisionID] {
			continue
		}
		seen[rev.RevisionID] = true
		c.Records = append(c.Records, FromRevision(rev))
	}

	classOrder := map[string]int{DocumentClass: len(contract.Classes)}
	for i, cl := range contract.Classes {
		classOrder[cl.Key] = i
	}
	sort.SliceStable(c.Records, func(i, j int) bool {
		a, b := c.Records[i], c.Records[j]
		if classOrder[a.Class] != classOrder[b.Class] {
			return classOrder[a.Class] < classOrder[b.Class]
		}
		if a.Class == DocumentClass && a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return KeyLess(a.Key, b.Key)
	})

	byKey := map[string][]*Record{}
	for _, r := range c.Records {
		if r.Key == "" {
			continue
		}
		byKey[r.Key] = append(byKey[r.Key], r)
	}
	for key, rs := range byKey {
		c.ByKey[key] = rs[0]
		if len(rs) > 1 {
			c.Duplicates[key] = rs
		}
	}

	for _, r := range c.Records {
		for _, rel := range r.Relationships {
			if rel.Malformed || rel.Target == "" {
				continue
			}
			t := NormalizeTarget(rel.Target)
			c.inbound[t] = append(c.inbound[t], Link{From: r, Via: "relationship", Type: rel.Type, Note: rel.Note})
		}
		for _, w := range r.Wikilinks {
			t := NormalizeTarget(w)
			c.inbound[t] = append(c.inbound[t], Link{From: r, Via: "wikilink"})
		}
	}
	return c
}

// NormalizeTarget maps a relationship target or wikilink to the key it names:
// it trims, strips the `atlas:` prefix the agreed target grammar uses, and
// normalizes a loosely typed Atlas key.
func NormalizeTarget(target string) string {
	t := strings.TrimSpace(target)
	t = strings.TrimPrefix(t, "atlas:")
	return contract.NormalizeID(t)
}

// Resolve returns the record a relationship target or wikilink names.
func (c *Corpus) Resolve(target string) (*Record, bool) {
	r, ok := c.ByKey[NormalizeTarget(target)]
	return r, ok
}

// Inbound returns everything that references the record with the given key.
func (c *Corpus) Inbound(key string) []Link { return c.inbound[key] }

// OfClass returns the records of one class (or DocumentClass), in order.
func (c *Corpus) OfClass(class string) []*Record {
	var out []*Record
	for _, r := range c.Records {
		if r.Class == class {
			out = append(out, r)
		}
	}
	return out
}

// ClassRecords returns every record that belongs to one of the seven classes.
func (c *Corpus) ClassRecords() []*Record {
	var out []*Record
	for _, r := range c.Records {
		if !r.IsDocument() {
			out = append(out, r)
		}
	}
	return out
}

// Suggest returns up to n existing keys that look like a key that was not
// found. It matches on the letters and digits alone, so `ATLAS-INIT-014` finds
// the root document `atlas/init-014-harnessed-agent-runtimes` that holds the
// Initiative that ought to be at that key.
func (c *Corpus) Suggest(key string, n int) []string {
	q := squash(key)
	if len(q) < 4 {
		return nil
	}
	type candidate struct {
		key  string
		dist int
	}
	var found []candidate
	for _, r := range c.Records {
		k := squash(r.Key)
		if len(k) < 4 || k == q {
			continue
		}
		if strings.Contains(k, q) || strings.Contains(q, k) {
			d := len(k) - len(q)
			if d < 0 {
				d = -d
			}
			found = append(found, candidate{r.Key, d})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].dist != found[j].dist {
			return found[i].dist < found[j].dist
		}
		return found[i].key < found[j].key
	})
	var out []string
	for _, f := range found {
		if len(out) == n {
			break
		}
		out = append(out, f.key)
	}
	return out
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// KeyLess orders keys naturally: the same prefix compares by its trailing
// number, so ATLAS-Q-9 sorts before ATLAS-Q-10.
func KeyLess(a, b string) bool {
	pa, na, oka := splitNumber(a)
	pb, nb, okb := splitNumber(b)
	if oka && okb && pa == pb {
		return na < nb
	}
	return a < b
}

func splitNumber(k string) (string, int, bool) {
	i := strings.LastIndex(k, "-")
	if i < 0 {
		return k, 0, false
	}
	n, err := strconv.Atoi(k[i+1:])
	if err != nil {
		return k, 0, false
	}
	return k[:i], n, true
}

// Store loads the corpus and caches it for a short time, so a burst of tool
// calls reads Tesseract once. A ttl of zero disables the cache.
type Store struct {
	client *tesseract.Client
	ttl    time.Duration
	now    func() time.Time

	mu  sync.Mutex
	cur *Corpus
}

// NewStore returns a Store reading through client.
func NewStore(client *tesseract.Client, ttl time.Duration) *Store {
	return &Store{client: client, ttl: ttl, now: time.Now}
}

// Get returns the corpus, reloading when the cache is empty, expired, or
// refresh is set. If a reload fails while a previous corpus exists, that one
// is returned marked Stale rather than failing the call: for a read-only view
// an older answer that says it is older beats no answer.
func (s *Store) Get(ctx context.Context, refresh bool) (*Corpus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cur != nil && !refresh && s.ttl > 0 && s.now().Sub(s.cur.LoadedAt) < s.ttl {
		return s.cur, nil
	}
	fresh, err := s.load(ctx)
	if err != nil {
		if s.cur == nil {
			return nil, err
		}
		stale := *s.cur
		stale.Stale, stale.StaleReason = true, err.Error()
		return &stale, nil
	}
	s.cur = fresh
	return fresh, nil
}

func (s *Store) load(ctx context.Context) (*Corpus, error) {
	revs, complete, err := s.client.RecallAll(ctx, tesseract.RecallRequest{
		// The glob does not include the root namespace itself, which holds
		// documents of its own, so the root is named separately.
		Namespaces:  []string{contract.Root, contract.Root + "/*"},
		Ranking:     "chronological",
		PayloadMode: "full",
	}, maxRecords)
	if err != nil {
		return nil, err
	}
	c := Build(revs, s.now())
	if !complete {
		c.Warnings = append(c.Warnings, fmt.Sprintf("loaded %d records and stopped at the %d-record cap; what follows is a partial view", len(revs), maxRecords))
	}
	return c, nil
}
