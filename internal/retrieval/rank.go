package retrieval

import "sort"

type Hit struct {
	ChunkID   string
	Retriever string
	Rank      int
	Score     float64
	Weight    float64
}
type ParentScore struct {
	ParentID string
	Score    float64
	ChildIDs []string
}

// WeightedRRF merges independent retrievers without allowing a single route
// to dominate solely by candidate count.
func WeightedRRF(routes []Hit, k float64) map[string]float64 {
	if k <= 0 {
		k = 60
	}
	out := map[string]float64{}
	for _, h := range routes {
		w := h.Weight
		if w <= 0 {
			w = 1
		}
		out[h.ChunkID] += w / (k + float64(h.Rank))
	}
	return out
}
func AggregateParents(hits []Hit, chunks map[string]Chunk, maxChildren int) []ParentScore {
	if maxChildren <= 0 {
		maxChildren = 3
	}
	type item struct {
		id    string
		score float64
	}
	by := map[string]map[string]item{}
	for _, h := range hits {
		c, ok := chunks[h.ChunkID]
		if !ok {
			continue
		}
		pid := c.ParentID
		if pid == "" {
			pid = c.ID
		}
		if by[pid] == nil {
			by[pid] = map[string]item{}
		}
		if old, exists := by[pid][h.ChunkID]; !exists || h.Score > old.score {
			by[pid][h.ChunkID] = item{h.ChunkID, h.Score}
		}
	}
	out := make([]ParentScore, 0, len(by))
	for pid, im := range by {
		items := make([]item, 0, len(im))
		for _, v := range im {
			items = append(items, v)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].score == items[j].score {
				return items[i].id < items[j].id
			}
			return items[i].score > items[j].score
		})
		if len(items) > maxChildren {
			items = items[:maxChildren]
		}
		p := ParentScore{ParentID: pid}
		for _, it := range items {
			p.Score += it.score
			p.ChildIDs = append(p.ChildIDs, it.id)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ParentID < out[j].ParentID
		}
		return out[i].Score > out[j].Score
	})
	return out
}
