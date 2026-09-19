package corpus

import (
	"sort"
	"strings"

	"github.com/hollis-labs/station/internal/atlas/contract"
)

// TagCounts counts how many records carry each tag.
func (c *Corpus) TagCounts() map[string]int {
	counts := map[string]int{}
	for _, r := range c.Records {
		for _, t := range r.Tags {
			counts[t]++
		}
	}
	return counts
}

// TagSquash reduces a tag to the form two spellings of one thing share:
// lowercase, without separators, without a plural s.
func TagSquash(tag string) string {
	k := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(tag))
	if len(k) > 4 {
		k = strings.TrimSuffix(k, "s")
	}
	return k
}

// NearDuplicateTags groups the system tags in counts that differ only in
// spelling (agentkit / agent-kit). Each group is sorted; the groups are
// ordered by their squashed form.
func NearDuplicateTags(counts map[string]int) [][]string {
	groups := map[string][]string{}
	for t := range counts {
		if contract.TagKind(t) != contract.TagSystem {
			continue
		}
		k := TagSquash(t)
		groups[k] = append(groups[k], t)
	}
	var keys []string
	for k, g := range groups {
		if len(g) > 1 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([][]string, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		sort.Strings(g)
		out = append(out, g)
	}
	return out
}
