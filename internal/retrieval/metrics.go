package retrieval

import (
	"math"
	"sort"
)

type Metrics struct{ Recall, Precision, MRR, NDCG, Coverage, DuplicateRate, Novelty float64 }

func RankMetrics(ids []string, relevance map[string]int, k int) Metrics {
	requestedK := k
	if k <= 0 {
		k = len(ids)
		requestedK = k
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(ids))
	dup := 0
	for _, id := range ids {
		if seen[id] {
			dup++
		} else {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	ids = unique
	if k > len(ids) {
		k = len(ids)
	}
	if k == 0 {
		return Metrics{}
	}
	total := 0
	for _, v := range relevance {
		if v > 0 {
			total++
		}
	}
	hits := 0
	first := 0
	dcg := 0.0
	for i, id := range ids[:k] {
		r := relevance[id]
		if r > 0 {
			hits++
			if first == 0 {
				first = i + 1
			}
		}
		if r > 0 {
			dcg += float64(r) / log2(i+2)
		}
	}
	ideal := make([]int, 0, len(relevance))
	for _, v := range relevance {
		ideal = append(ideal, v)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, v := range ideal[:min(k, len(ideal))] {
		if v > 0 {
			idcg += float64(v) / log2(i+2)
		}
	}
	denom := requestedK
	if denom <= 0 {
		denom = 1
	}
	m := Metrics{Precision: float64(hits) / float64(denom), MRR: 0, NDCG: 0, DuplicateRate: float64(dup) / float64(dup+len(ids))}
	if total > 0 {
		m.Recall = float64(hits) / float64(total)
	}
	if first > 0 {
		m.MRR = 1 / float64(first)
	}
	if idcg > 0 {
		m.NDCG = dcg / idcg
	}
	return m
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func log2(v int) float64 { return math.Log2(float64(v)) }
