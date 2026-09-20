package contextmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/schema"
)

type Window struct {
	MaxApproxTokens int
	ReserveTokens   int
	MinTailMessages int
	RecentMessages  int
	MaxToolBytes    int
	Archive         Archiver
}

type Archiver interface {
	ArchiveMessages(context.Context, []schema.Message) (map[int]string, error)
}

func (w Window) Compact(ctx context.Context, messages []schema.Message) ([]schema.Message, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if w.MaxApproxTokens <= 0 {
		return nil, false, errors.New("context window: max approximate tokens must be positive")
	}
	budget := w.MaxApproxTokens - w.ReserveTokens
	if budget <= 0 {
		return nil, false, errors.New("context window: reserve consumes the full budget")
	}
	if approximateTokens(messages) <= budget || len(messages) <= 2 {
		return append([]schema.Message(nil), messages...), false, nil
	}
	references := map[int]string{}
	if w.Archive != nil {
		var err error
		references, err = w.Archive.ArchiveMessages(ctx, messages)
		if err != nil {
			return nil, false, fmt.Errorf("archive context before compaction: %w", err)
		}
	}
	working := append([]schema.Message(nil), messages...)
	recent := w.RecentMessages
	if recent <= 0 {
		recent = 8
	}
	recentStart := len(working) - recent
	if recentStart < 1 {
		recentStart = 1
	}
	maxToolBytes := w.MaxToolBytes
	if maxToolBytes <= 0 {
		maxToolBytes = 4 << 10
	}
	changed := false
	for index := 1; index < recentStart; index++ {
		message := &working[index]
		switch message.Role {
		case schema.RoleTool:
			if message.Content != "" && !strings.HasPrefix(message.Content, "[context archived") {
				message.Content = archiveMarker("tool result", references[index])
				changed = true
			}
		case schema.RoleAssistant:
			if message.Content != "" && len(message.Content) > 256 {
				message.Content = archiveMarker("assistant reasoning", references[index])
				changed = true
			}
		}
	}
	for index := recentStart; index < len(working); index++ {
		message := &working[index]
		if message.Role == schema.RoleTool && len(message.Content) > maxToolBytes {
			message.Content = headTail(message.Content, maxToolBytes, references[index])
			changed = true
		}
	}
	if approximateTokens(working) <= budget {
		return working, changed, nil
	}
	for index := 1; index < recentStart; index++ {
		message := &working[index]
		if message.Role == schema.RoleAssistant && message.Content != "" && !strings.HasPrefix(message.Content, "[context archived") {
			message.Content = archiveMarker("assistant message", references[index])
			changed = true
		}
	}
	if approximateTokens(working) <= budget {
		return working, changed, nil
	}

	minimum := w.MinTailMessages
	if minimum <= 0 {
		minimum = 4
	}
	notice := schema.Message{Role: schema.RoleSystem, Content: "Earlier conversation messages were archived and compacted by the harness. Use recall_context with a focused query to retrieve exact details. Newer raw messages override older raw messages, which override summaries."}
	fixed := approximateTokens(working[:1]) + approximateTokens([]schema.Message{notice})
	start := len(working)
	used := fixed
	for start > 1 {
		candidate := approximateTokens(working[start-1 : start])
		if len(working)-start >= minimum && used+candidate > budget {
			break
		}
		start--
		used += candidate
	}
	start = includeToolCallOwner(working, start, 1)
	if start <= 1 {
		return working, changed, nil
	}
	result := make([]schema.Message, 0, 2+len(working)-start)
	result = append(result, working[0])
	notice.Content += fmt.Sprintf(" Omitted %d message(s).", start-1)
	result = append(result, notice)
	result = append(result, working[start:]...)
	return result, true, nil
}

func archiveMarker(kind, sourceID string) string {
	if sourceID == "" {
		return fmt.Sprintf("[context archived: %s; use recall_context to recover exact details]", kind)
	}
	return fmt.Sprintf("[context archived: %s; source_id=%s; use recall_context to recover exact details]", kind, sourceID)
}

func headTail(content string, maximum int, sourceID string) string {
	if len(content) <= maximum {
		return content
	}
	marker := archiveMarker("large recent tool result", sourceID)
	remaining := maximum - len(marker) - 2
	if remaining < 64 {
		return marker
	}
	headSize := remaining / 2
	tailSize := remaining - headSize
	for headSize > 0 && !utf8.ValidString(content[:headSize]) {
		headSize--
	}
	tailStart := len(content) - tailSize
	for tailStart < len(content) && !utf8.ValidString(content[tailStart:]) {
		tailStart++
	}
	return content[:headSize] + "\n" + marker + "\n" + content[tailStart:]
}

func includeToolCallOwner(messages []schema.Message, start, floor int) int {
	if start >= len(messages) || messages[start].Role != schema.RoleTool {
		return start
	}
	needed := make(map[string]bool)
	for i := start; i < len(messages) && messages[i].Role == schema.RoleTool; i++ {
		needed[messages[i].ToolCallID] = true
	}
	for i := start - 1; i >= floor; i-- {
		if messages[i].Role != schema.RoleAssistant {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			delete(needed, call.ID)
		}
		if len(needed) == 0 {
			return i
		}
	}
	return start
}

func approximateTokens(messages []schema.Message) int {
	encoded, _ := json.Marshal(messages)
	return (len(encoded) + 3) / 4
}
