package memory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
)

const maximumArchiveContentBytes = 1 << 20

type Entry struct {
	ID         string            `json:"id"`
	Scope      string            `json:"scope"`
	RunID      string            `json:"run_id"`
	Turn       int               `json:"turn"`
	Index      int               `json:"index"`
	Role       schema.Role       `json:"role"`
	ToolCalls  []schema.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Content    string            `json:"content"`
	IsError    bool              `json:"is_error,omitempty"`
	Digest     string            `json:"digest"`
	ArchivedAt time.Time         `json:"archived_at"`
}

type SearchResult struct {
	SourceID   string            `json:"source_id"`
	Role       schema.Role       `json:"role"`
	ToolCalls  []schema.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Content    string            `json:"content"`
	IsError    bool              `json:"is_error,omitempty"`
	Score      float64           `json:"score"`
	RunID      string            `json:"run_id"`
	Turn       int               `json:"turn"`
}

type Archive struct {
	dir string
	mu  sync.Mutex
}

func NewArchive(dir string) (*Archive, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve memory directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create memory directory: %w", err)
	}
	return &Archive{dir: abs}, nil
}

// ArchiveMessages durably stores an idempotent copy of every message and
// returns source identifiers by original message index.
func (a *Archive) ArchiveMessages(ctx context.Context, messages []schema.Message) (map[int]string, error) {
	metadata, ok := runtimecontext.FromContext(ctx)
	if !ok || strings.TrimSpace(metadata.Scope) == "" || strings.TrimSpace(metadata.RunID) == "" {
		return nil, errors.New("memory archive requires run scope metadata")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(messages))
	references := make(map[int]string, len(messages))
	now := time.Now().UTC()
	for index, message := range messages {
		if message.Role == schema.RoleSystem || (strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0) {
			continue
		}
		if len(message.Content) > maximumArchiveContentBytes {
			return nil, fmt.Errorf("message %d exceeds archive limit", index)
		}
		identity, err := json.Marshal(struct {
			Scope      string            `json:"scope"`
			RunID      string            `json:"run_id"`
			Turn       int               `json:"turn"`
			Index      int               `json:"index"`
			Role       schema.Role       `json:"role"`
			ToolCalls  []schema.ToolCall `json:"tool_calls,omitempty"`
			ToolCallID string            `json:"tool_call_id"`
			Content    string            `json:"content"`
			IsError    bool              `json:"is_error"`
		}{metadata.Scope, metadata.RunID, metadata.Turn, index, message.Role, message.ToolCalls, message.ToolCallID, message.Content, message.IsError})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(identity)
		contentSum := sha256.Sum256(identity)
		entry := Entry{
			ID: "mem-" + hex.EncodeToString(sum[:16]), Scope: metadata.Scope, RunID: metadata.RunID,
			Turn: metadata.Turn, Index: index, Role: message.Role,
			ToolCalls: append([]schema.ToolCall(nil), message.ToolCalls...), ToolCallID: message.ToolCallID,
			Content: message.Content, IsError: message.IsError,
			Digest: hex.EncodeToString(contentSum[:]), ArchivedAt: now,
		}
		entries = append(entries, entry)
		references[index] = entry.ID
	}
	if len(entries) == 0 {
		return references, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	path := a.scopePath(metadata.Scope)
	existing, err := loadEntries(path)
	if err != nil {
		return nil, err
	}
	known := make(map[string]Entry, len(existing))
	for _, entry := range existing {
		known[entry.ID] = entry
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	encoder := json.NewEncoder(file)
	for _, entry := range entries {
		if previous, exists := known[entry.ID]; exists {
			if previous.Digest != entry.Digest || previous.Scope != entry.Scope {
				file.Close()
				return nil, fmt.Errorf("memory source %q has conflicting semantics", entry.ID)
			}
			continue
		}
		if err := encoder.Encode(entry); err != nil {
			file.Close()
			return nil, err
		}
		known[entry.ID] = entry
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return nil, err
	}
	directory, err := os.Open(a.dir)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return nil, err
	}
	verified, err := loadEntries(path)
	if err != nil {
		return nil, fmt.Errorf("read back memory archive: %w", err)
	}
	verifiedIDs := make(map[string]bool, len(verified))
	for _, entry := range verified {
		verifiedIDs[entry.ID] = true
	}
	for _, entry := range entries {
		if !verifiedIDs[entry.ID] {
			return nil, fmt.Errorf("memory source %q missing after commit", entry.ID)
		}
	}
	return references, nil
}

func (a *Archive) Search(ctx context.Context, scope, query string, limit int) ([]SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if scope == "" || query == "" {
		return nil, errors.New("memory scope and query are required")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	a.mu.Lock()
	entries, err := loadEntries(a.scopePath(scope))
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	queryTerms := termFrequency(tokenize(query))
	if len(queryTerms) == 0 {
		return nil, errors.New("query has no searchable terms")
	}
	documents := make([]map[string]int, len(entries))
	documentFrequency := make(map[string]int)
	totalLength := 0
	for index, entry := range entries {
		documents[index] = termFrequency(tokenize(searchableText(entry)))
		length := 0
		for _, count := range documents[index] {
			length += count
		}
		totalLength += length
		for term := range documents[index] {
			documentFrequency[term]++
		}
	}
	averageLength := float64(totalLength) / float64(len(entries))
	if averageLength < 1 {
		averageLength = 1
	}
	results := make([]SearchResult, 0, len(entries))
	const k1, b = 1.2, 0.75
	for index, entry := range entries {
		documentLength := 0
		for _, count := range documents[index] {
			documentLength += count
		}
		score := 0.0
		for term, queryCount := range queryTerms {
			frequency := documents[index][term]
			if frequency == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(entries)-documentFrequency[term])+0.5)/(float64(documentFrequency[term])+0.5))
			denominator := float64(frequency) + k1*(1-b+b*float64(documentLength)/averageLength)
			score += float64(queryCount) * idf * (float64(frequency) * (k1 + 1) / denominator)
		}
		if score > 0 {
			results = append(results, SearchResult{
				SourceID: entry.ID, Role: entry.Role, ToolCalls: entry.ToolCalls, ToolCallID: entry.ToolCallID,
				Content: boundedExcerpt(entry.Content, query, 2048), IsError: entry.IsError,
				Score: score, RunID: entry.RunID, Turn: entry.Turn,
			})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			if results[i].Turn == results[j].Turn {
				return results[i].SourceID < results[j].SourceID
			}
			return results[i].Turn > results[j].Turn
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func loadEntries(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	var entries []Entry
	seen := make(map[string]string)
	for {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode memory archive: %w", err)
		}
		if entry.ID == "" || entry.Scope == "" || entry.RunID == "" || entry.Digest == "" || entry.ArchivedAt.IsZero() {
			return nil, errors.New("memory archive contains an invalid entry")
		}
		if previous, exists := seen[entry.ID]; exists {
			if previous != entry.Digest {
				return nil, fmt.Errorf("memory archive contains conflicting source %q", entry.ID)
			}
			continue
		}
		seen[entry.ID] = entry.Digest
		entries = append(entries, entry)
	}
	return entries, nil
}

func termFrequency(tokens []string) map[string]int {
	result := make(map[string]int, len(tokens))
	for _, token := range tokens {
		result[token]++
	}
	return result
}

func searchableText(entry Entry) string {
	if len(entry.ToolCalls) == 0 {
		return entry.Content
	}
	encoded, _ := json.Marshal(entry.ToolCalls)
	return entry.Content + " " + string(encoded)
}

func boundedExcerpt(content, query string, maximum int) string {
	if len(content) <= maximum {
		return content
	}
	lower := strings.ToLower(content)
	position := strings.Index(lower, strings.ToLower(strings.TrimSpace(query)))
	if position < 0 {
		for _, token := range tokenize(query) {
			if len(token) < 2 {
				continue
			}
			if candidate := strings.Index(lower, token); candidate >= 0 && (position < 0 || candidate < position) {
				position = candidate
			}
		}
	}
	if position < 0 {
		position = 0
	}
	start := position - maximum/3
	if start < 0 {
		start = 0
	}
	end := start + maximum
	if end > len(content) {
		end = len(content)
		start = end - maximum
		if start < 0 {
			start = 0
		}
	}
	for start < end && !utf8.RuneStart(content[start]) {
		start++
	}
	for end > start && end < len(content) && !utf8.RuneStart(content[end]) {
		end--
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(content) {
		suffix = "…"
	}
	return prefix + content[start:end] + suffix
}

func tokenize(value string) []string {
	value = strings.ToLower(value)
	var tokens []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	var previousHan rune
	for _, item := range value {
		switch {
		case unicode.Is(unicode.Han, item):
			flush()
			tokens = append(tokens, string(item))
			if previousHan != 0 {
				tokens = append(tokens, string([]rune{previousHan, item}))
			}
			previousHan = item
		case unicode.IsLetter(item) || unicode.IsDigit(item) || item == '_' || item == '-':
			previousHan = 0
			word = append(word, item)
		default:
			previousHan = 0
			flush()
		}
	}
	flush()
	return tokens
}

func (a *Archive) scopePath(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(a.dir, hex.EncodeToString(sum[:])+".memory.jsonl")
}
