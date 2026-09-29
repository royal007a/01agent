package retrieval

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Searcher struct {
	WorkDir  string
	MaxFiles int
}
type SearchResult struct {
	Chunk     Chunk
	Parent    *Chunk
	Score     float64
	Retriever string
	Rank      int
}

func (s *Searcher) Search(query string, k int) []SearchResult {
	if k <= 0 {
		k = 8
	}
	terms := terms(query)
	var all []SearchResult
	var parents = map[string]Chunk{}
	files := 0
	_ = filepath.Walk(s.WorkDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || files >= s.MaxFiles {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+".claw"+string(filepath.Separator)) {
			return nil
		}
		rel, _ := filepath.Rel(s.WorkDir, path)
		f, e := os.OpenInRoot(s.WorkDir, rel)
		if e != nil {
			return nil
		}
		b, e := io.ReadAll(io.LimitReader(f, 2<<20+1))
		f.Close()
		if e != nil || len(b) > 2<<20 {
			return nil
		}
		files++
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		typ := ext
		if ext == "md" {
			typ = "markdown"
		}
		cs := ChunkStructured("workspace", rel, b2s(b), ChunkOptions{Type: typ, MaxChildTokens: 300, MaxParentTokens: 2000})
		for _, c := range cs {
			if c.Kind == "parent" {
				parents[c.ID] = c
			}
		}
		for _, c := range cs {
			if c.Kind != "child" {
				continue
			}
			score := tokenScore(c.Text, terms)
			if score > 0 {
				p := parents[c.ParentID]
				all = append(all, SearchResult{Chunk: c, Parent: &p, Score: score, Retriever: "bm25", Rank: 0})
			}
		}
		return nil
	})
	// Add a deterministic exact lane and merge with weighted RRF. The exact
	// lane is intentionally cheap and only boosts identifier/path-like queries.
	exact := make([]Hit, 0, len(all))
	bm := append([]SearchResult(nil), all...)
	sort.SliceStable(bm, func(i, j int) bool {
		if bm[i].Score == bm[j].Score {
			return bm[i].Chunk.ID < bm[j].Chunk.ID
		}
		return bm[i].Score > bm[j].Score
	})
	for i, r := range all {
		if exactScore(r.Chunk.Text, query) > 0 {
			exact = append(exact, Hit{ChunkID: r.Chunk.ID, Retriever: "rg", Rank: i + 1, Weight: 1.5})
		}
	}
	base := make([]Hit, 0, len(bm))
	for i, r := range bm {
		base = append(base, Hit{ChunkID: r.Chunk.ID, Retriever: "bm25", Rank: i + 1, Weight: 1})
	}
	scores := WeightedRRF(append(base, exact...), 60)
	for i := range all {
		all[i].Score = scores[all[i].Chunk.ID]
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score == all[j].Score {
			return all[i].Chunk.ID < all[j].Chunk.ID
		}
		return all[i].Score > all[j].Score
	})
	if len(all) > k {
		all = all[:k]
	}
	for i := range all {
		all[i].Rank = i + 1
	}
	return all
}
func b2s(b []byte) string { return string(b) }
func terms(q string) []string {
	f := regexp.MustCompile(`[A-Za-z0-9_./:-]+`)
	out := f.FindAllString(strings.ToLower(q), -1)
	for _, w := range strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		r := []rune(w)
		for i := 0; i+1 < len(r); i++ {
			if r[i] >= 128 || r[i+1] >= 128 {
				out = append(out, string(r[i:i+2]))
			}
		}
	}
	return out
}
func tokenScore(text string, ts []string) float64 {
	l := strings.ToLower(text)
	score := 0.0
	for _, t := range ts {
		if strings.Contains(l, strings.ToLower(t)) {
			score += 1
		}
	}
	return score / (1 + float64(len([]rune(text)))/1000)
}

func exactScore(text, query string) float64 {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0
	}
	if strings.Contains(strings.ToLower(text), q) {
		return 1
	}
	return 0
}

// ReadLines is the exact-match lane used as the rg-equivalent path for tokens
// where semantic scoring is weak (identifiers, paths and error codes).
func ReadLines(root, query string) []string {
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		f, e := os.OpenInRoot(root, rel)
		if e != nil {
			return nil
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.Contains(strings.ToLower(sc.Text()), strings.ToLower(query)) {
				out = append(out, path+":"+sc.Text())
			}
		}
		f.Close()
		return nil
	})
	return out
}
