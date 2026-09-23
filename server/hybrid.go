package main

import (
	"sort"
)

// HybridHit is one result of hybrid search: the current memory at a keypath,
// its reciprocal-rank-fusion score, and the search modes that returned it.
type HybridHit struct {
	*Memory
	Score   float32  `json:"score"`
	Sources []string `json:"sources"`
}

// rrfK is the reciprocal rank fusion constant. 60 is the value from the
// original paper (Cormack et al. 2009) and damps the gap between rank 1
// and rank 2 so that a hit found by both lists beats a hit found by one.
const rrfK = 60

// rrfFuse merges the FTS ranking and the semantic ranking into one list by
// reciprocal rank fusion: each list contributes 1/(rrfK+rank) for every item
// it holds, ranks are 1-based, and items are keyed by project and keypath
// because a search with no project spans the whole store. Ties break on
// project then keypath so the output is deterministic. The result holds at
// most limit items.
func rrfFuse(fts []*Memory, sem []*SemanticHit, limit int) []*HybridHit {
	if limit <= 0 {
		limit = 20
	}
	type key struct{ project, keypath string }
	byKey := map[key]*HybridHit{}
	var order []key
	add := func(m *Memory, rank int, source string) {
		k := key{m.ProjectID, m.Keypath}
		h, ok := byKey[k]
		if !ok {
			h = &HybridHit{Memory: m}
			byKey[k] = h
			order = append(order, k)
		}
		h.Score += 1 / float32(rrfK+rank)
		h.Sources = append(h.Sources, source)
	}
	for i, m := range fts {
		add(m, i+1, "fts")
	}
	for i, s := range sem {
		add(s.Memory, i+1, "semantic")
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byKey[order[i]], byKey[order[j]]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.Keypath < b.Keypath
	})
	out := make([]*HybridHit, 0, min(len(order), limit))
	for _, k := range order {
		if len(out) == limit {
			break
		}
		out = append(out, byKey[k])
	}
	return out
}
