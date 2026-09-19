package contextmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/royal007a/01agent/internal/schema"
)

type Window struct {
	MaxApproxTokens int
	ReserveTokens   int
	MinTailMessages int
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
	minimum := w.MinTailMessages
	if minimum <= 0 {
		minimum = 4
	}
	prefixEnd := 2
	if prefixEnd > len(messages) {
		prefixEnd = len(messages)
	}
	notice := schema.Message{Role: schema.RoleSystem, Content: "Earlier conversation messages were compacted by the harness. Durable run traces and checkpoints retain the original history. Re-read workspace evidence when exact details are needed."}
	fixed := approximateTokens(messages[:prefixEnd]) + approximateTokens([]schema.Message{notice})
	start := len(messages)
	used := fixed
	for start > prefixEnd {
		candidate := approximateTokens(messages[start-1 : start])
		if len(messages)-start >= minimum && used+candidate > budget {
			break
		}
		start--
		used += candidate
	}
	start = includeToolCallOwner(messages, start, prefixEnd)
	if start <= prefixEnd {
		return append([]schema.Message(nil), messages...), false, nil
	}
	result := make([]schema.Message, 0, prefixEnd+1+len(messages)-start)
	result = append(result, messages[:prefixEnd]...)
	notice.Content += fmt.Sprintf(" Omitted %d message(s).", start-prefixEnd)
	result = append(result, notice)
	result = append(result, messages[start:]...)
	return result, true, nil
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
