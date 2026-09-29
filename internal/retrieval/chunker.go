package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"unicode/utf8"
)

type ChunkOptions struct {
	MaxChildTokens, MaxParentTokens, Depth int
	Type                                   string
}

func EstimateTokens(s string) int {
	n := 0
	for _, r := range s {
		if r < 128 {
			n++
		} else {
			n += 4
		}
	}
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

type sourcePart struct {
	start, end int
	text       string
}

// ChunkStructured creates bounded parent sections and packed child chunks.
// IDs include occurrence ordinal so repeated lines remain distinct.
func ChunkStructured(source, path, text string, o ChunkOptions) []Chunk {
	if o.MaxChildTokens <= 0 {
		o.MaxChildTokens = 300
	}
	if o.MaxParentTokens <= 0 {
		o.MaxParentTokens = 2000
	}
	if o.Depth <= 0 {
		o.Depth = 8
	}
	parts := structureParts(text, o.Type)
	out := []Chunk{}
	occ := map[string]int{}
	for _, section := range parts {
		kids := packParts(sentenceParts(section), o.MaxChildTokens, 80, text)
		if len(kids) == 0 {
			continue
		}
		for i := 0; i < len(kids); {
			j := i
			n := 0
			for j < len(kids) && n+partTokens(text, kids[j]) <= o.MaxParentTokens {
				n += partTokens(text, kids[j])
				j++
			}
			if j == i {
				j++
			}
			group := kids[i:j]
			parentText := text[group[0].start:group[len(group)-1].end]
			pid := stableID(source, path, parentText, occ)
			out = append(out, makeChunk(source, path, text, sourcePart{group[0].start, group[len(group)-1].end, parentText}, "", "parent", pid))
			for _, k := range group {
				cid := stableID(source, path, text[k.start:k.end], occ)
				out = append(out, makeChunk(source, path, text, k, pid, "child", cid))
			}
			i = j
		}
	}
	return out
}

func structureParts(s, typ string) []sourcePart {
	if typ == "go" {
		if p := goParts(s); len(p) > 0 {
			return p
		}
	}
	if typ == "markdown" {
		var out []sourcePart
		start, off := 0, 0
		fence := false
		for _, line := range strings.SplitAfter(s, "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "```") {
				fence = !fence
			}
			if !fence && strings.HasPrefix(trim, "#") && off > start {
				out = append(out, sourcePart{start, off, s[start:off]})
				start = off
			}
			off += len(line)
		}
		if start < len(s) {
			out = append(out, sourcePart{start, len(s), s[start:]})
		}
		return mergeSmall(out, s, 80)
	}
	if typ == "log" {
		return logParts(s)
	}
	return mergeSmall(paragraphParts(s), s, 80)
}
func goParts(s string) []sourcePart {
	f := token.NewFileSet()
	file, err := parser.ParseFile(f, "x.go", s, parser.ParseComments)
	if err != nil {
		return nil
	}
	var out []sourcePart
	if len(file.Decls) == 0 {
		return nil
	}
	first := f.Position(file.Decls[0].Pos()).Offset
	if first > 0 {
		out = append(out, sourcePart{0, first, s[:first]})
	}
	for _, d := range file.Decls {
		a := f.Position(d.Pos()).Offset
		b := f.Position(d.End()).Offset
		if gd, ok := d.(*ast.FuncDecl); ok && gd.Doc != nil {
			a = f.Position(gd.Doc.Pos()).Offset
		}
		if a >= 0 && b <= len(s) {
			out = append(out, sourcePart{a, b, s[a:b]})
		}
	}
	return out
}

var logPrefix = regexp.MustCompile(`^(\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}|\[[0-9]{4}[-/]\d{2}[-/]\d{2})`)

func logParts(s string) []sourcePart {
	var records []sourcePart
	start, off, n := 0, 0, 0
	for _, line := range strings.SplitAfter(s, "\n") {
		if off > start && logPrefix.MatchString(strings.TrimSpace(line)) {
			records = append(records, sourcePart{start, off, s[start:off]})
			start = off
			n++
			if n%50 == 0 {
			}
		}
		off += len(line)
	}
	if start < len(s) {
		records = append(records, sourcePart{start, len(s), s[start:]})
	}
	var out []sourcePart
	for i := 0; i < len(records); i += 50 {
		j := i + 50
		if j > len(records) {
			j = len(records)
		}
		out = append(out, sourcePart{records[i].start, records[j-1].end, s[records[i].start:records[j-1].end]})
	}
	return out
}
func paragraphParts(s string) []sourcePart {
	var out []sourcePart
	start := 0
	for start < len(s) {
		rel := strings.Index(s[start:], "\n\n")
		end := len(s)
		if rel >= 0 {
			end = start + rel
		}
		if strings.TrimSpace(s[start:end]) != "" {
			out = append(out, sourcePart{start, end, s[start:end]})
		}
		if rel < 0 {
			break
		}
		start = end
		for start < len(s) && (s[start] == '\n' || s[start] == '\r') {
			start++
		}
	}
	return out
}
func mergeSmall(in []sourcePart, full string, min int) []sourcePart {
	var out []sourcePart
	for _, p := range in {
		if len(out) > 0 && EstimateTokens(p.text) < min {
			out[len(out)-1].end = p.end
			out[len(out)-1].text = full[out[len(out)-1].start:p.end]
		} else {
			out = append(out, p)
		}
	}
	return out
}
func sentenceParts(p sourcePart) []sourcePart {
	var out []sourcePart
	start := p.start
	for i, r := range p.text {
		if strings.ContainsRune("。！？.!?\n", r) {
			end := p.start + i + utf8.RuneLen(r)
			if strings.TrimSpace(p.text[start-p.start:end-p.start]) != "" {
				out = append(out, sourcePart{start, end, p.text[start-p.start : end-p.start]})
			}
			start = end
		}
	}
	if start < p.end {
		out = append(out, sourcePart{start, p.end, p.text[start-p.start:]})
	}
	return out
}
func partTokens(full string, p sourcePart) int { return EstimateTokens(full[p.start:p.end]) }
func packParts(parts []sourcePart, max, min int, full string) []sourcePart {
	var out []sourcePart
	for _, p := range parts {
		if partTokens(full, p) > max {
			subs := sentenceParts(p)
			if len(subs) < 2 {
				subs = windowParts(p, max)
			}
			out = append(out, packParts(subs, max, min, full)...)
			continue
		}
		if len(out) > 0 && partTokens(full, p) < min && partTokens(full, out[len(out)-1])+partTokens(full, p) <= max {
			out[len(out)-1].end = p.end
			out[len(out)-1].text = out[len(out)-1].text + p.text
			continue
		}
		if len(out) > 0 && partTokens(full, out[len(out)-1])+partTokens(full, p) <= max {
			out[len(out)-1].end = p.end
			out[len(out)-1].text += p.text
		} else {
			out = append(out, p)
		}
	}
	final := make([]sourcePart, 0, len(out))
	for _, x := range out {
		if partTokens(full, x) > max {
			final = append(final, windowParts(x, max)...)
		} else {
			final = append(final, x)
		}
	}
	return final
}
func windowParts(p sourcePart, max int) []sourcePart {
	var out []sourcePart
	start := p.start
	tokens := 0
	limit := max * 4
	for start < p.end {
		end := start
		tokens = 0
		for end < p.end {
			r, n := utf8.DecodeRuneInString(p.text[end-p.start:])
			if r == utf8.RuneError && n == 1 {
				n = 1
			}
			add := 1
			if r >= 128 {
				add = 4
			}
			if tokens+add > limit && end > start {
				break
			}
			tokens += add
			end += n
		}
		if end == start {
			_, n := utf8.DecodeRuneInString(p.text[start-p.start:])
			end += n
		}
		out = append(out, sourcePart{start, end, p.text[start-p.start : end-p.start]})
		if end == p.end {
			break
		}
		back := 0
		target := limit / 10
		for end-back > start && back < target {
			_, n := utf8.DecodeLastRuneInString(p.text[:end-back-p.start])
			if n <= 0 {
				break
			}
			back += n
		}
		start = end - back
	}
	return out
}
func makeChunk(source, path, full string, p sourcePart, parentID, kind, id string) Chunk {
	startLine := func(off int) int {
		if off <= 0 {
			return 1
		}
		if off > len(full) {
			off = len(full)
		}
		return 1 + strings.Count(full[:off], "\n")
	}
	h := sha256.Sum256([]byte(full[p.start:p.end]))
	endLine := startLine(p.end)
	if p.end > p.start {
		endLine = startLine(p.end - 1)
	}
	return Chunk{ID: id, ParentID: parentID, Source: source, Path: path, Text: full[p.start:p.end], StartLine: startLine(p.start), EndLine: endLine, StartOffset: p.start, EndOffset: p.end, Kind: kind, ContentHash: hex.EncodeToString(h[:]), Trust: 1}
}
func chunkID(source, path, text string, ordinal int) string {
	h := sha256.Sum256([]byte(source + "|" + path + "|" + text + "|" + string(rune(ordinal))))
	return hex.EncodeToString(h[:])
}
func stableID(source, path, text string, occ map[string]int) string {
	h := sha256.Sum256([]byte(text))
	key := hex.EncodeToString(h[:])
	n := occ[key]
	occ[key] = n + 1
	return chunkID(source, path, text, n)
}
func ChunkDocument(source, path, text string, maxChild, maxParent int) []Chunk {
	return ChunkStructured(source, path, text, ChunkOptions{MaxChildTokens: maxChild, MaxParentTokens: maxParent})
}
