package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/schema"
	"github.com/xeipuuv/gojsonschema"
)

type Registry interface {
	Register(tool BaseTool) error
	GetAvailableTools() []schema.ToolDefinition
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
	ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult
}

type RegistryOption func(*registryImpl)

func WithPermissionPolicy(policy PermissionPolicy) RegistryOption {
	return func(r *registryImpl) {
		if policy != nil {
			r.policy = policy
		}
	}
}

func WithMaxParallel(n int) RegistryOption {
	return func(r *registryImpl) {
		if n > 0 {
			r.maxParallel = n
		}
	}
}

func WithToolTimeout(timeout time.Duration) RegistryOption {
	return func(r *registryImpl) {
		if timeout > 0 {
			r.toolTimeout = timeout
		}
	}
}

type registryImpl struct {
	mu          sync.RWMutex
	tools       map[string]BaseTool
	policy      PermissionPolicy
	maxParallel int
	toolTimeout time.Duration
}

func NewRegistry(options ...RegistryOption) Registry {
	r := &registryImpl{
		tools:       make(map[string]BaseTool),
		policy:      ReadOnlyPolicy{},
		maxParallel: 4,
		toolTimeout: 30 * time.Second,
	}
	for _, option := range options {
		option(r)
	}
	return r
}

func (r *registryImpl) Register(tool BaseTool) error {
	if tool == nil {
		return errors.New("register tool: tool is nil")
	}
	name := strings.TrimSpace(tool.Name())
	if name == "" {
		return errors.New("register tool: name is empty")
	}
	definition := tool.Definition()
	if definition.Name != name {
		return fmt.Errorf("register tool %q: definition name %q does not match", name, definition.Name)
	}
	if definition.InputSchema == nil {
		return fmt.Errorf("register tool %q: input schema is nil", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("register tool %q: duplicate name", name)
	}
	r.tools[name] = tool
	return nil
}

func (r *registryImpl) GetAvailableTools() []schema.ToolDefinition {
	r.mu.RLock()
	definitions := make([]schema.ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, tool.Definition())
	}
	r.mu.RUnlock()

	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})
	return definitions
}

func (r *registryImpl) lookup(name string) (BaseTool, bool) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	return tool, ok
}

func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	if err := ctx.Err(); err != nil {
		return contextResult(call.ID, err)
	}

	tool, exists := r.lookup(call.Name)
	if !exists {
		return errorResult(call.ID, "unknown_tool", fmt.Sprintf("tool %q is not registered", call.Name), true, false)
	}
	definition := tool.Definition()

	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	}
	if err := validateSchema(definition.InputSchema, call.Arguments); err != nil {
		return errorResult(call.ID, "schema_validation", err.Error(), true, false)
	}
	if err := tool.Validate(call.Arguments); err != nil {
		return errorResult(call.ID, "argument_validation", err.Error(), true, false)
	}

	decision := r.policy.CanUse(ctx, definition, call.Arguments)
	if !decision.Allowed {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "tool use denied by policy"
		}
		code := "permission_denied"
		if decision.RequiresApproval {
			code = "approval_required"
		}
		return errorResult(call.ID, code, reason, false, true)
	}

	toolCtx := ctx
	cancel := func() {}
	if r.toolTimeout > 0 {
		toolCtx, cancel = context.WithTimeout(ctx, r.toolTimeout)
	}
	defer cancel()

	output, err := executeSafely(toolCtx, tool, call.Arguments)
	if err == nil {
		return schema.ToolResult{ToolCallID: call.ID, Output: output}
	}
	if toolCtx.Err() != nil {
		return contextResult(call.ID, toolCtx.Err())
	}

	var toolErr *Error
	if errors.As(err, &toolErr) {
		return errorResult(call.ID, toolErr.Code, toolErr.Message, toolErr.Retryable, toolErr.Fatal)
	}
	return errorResult(call.ID, "tool_error", err.Error(), true, false)
}

func (r *registryImpl) ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult {
	results := make([]schema.ToolResult, len(calls))
	if len(calls) == 0 {
		return results
	}
	if !r.canRunInParallel(calls) {
		for i, call := range calls {
			results[i] = r.Execute(ctx, call)
			if results[i].Fatal {
				for j := i + 1; j < len(calls); j++ {
					results[j] = errorResult(calls[j].ID, "batch_cancelled", "not executed after a fatal tool result", false, true)
				}
				break
			}
		}
		return results
	}

	semaphore := make(chan struct{}, r.maxParallel)
	var wait sync.WaitGroup
	for i, call := range calls {
		wait.Add(1)
		go func(index int, item schema.ToolCall) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
				results[index] = r.Execute(ctx, item)
			case <-ctx.Done():
				results[index] = contextResult(item.ID, ctx.Err())
			}
		}(i, call)
	}
	wait.Wait()
	return results
}

func (r *registryImpl) canRunInParallel(calls []schema.ToolCall) bool {
	if len(calls) < 2 || r.maxParallel < 2 {
		return false
	}
	for _, call := range calls {
		tool, exists := r.lookup(call.Name)
		if !exists {
			return false
		}
		definition := tool.Definition()
		if definition.Risk != schema.RiskRead || !definition.ParallelSafe {
			return false
		}
	}
	return true
}

func validateSchema(inputSchema map[string]any, arguments json.RawMessage) error {
	result, err := gojsonschema.Validate(
		gojsonschema.NewGoLoader(inputSchema),
		gojsonschema.NewBytesLoader(arguments),
	)
	if err != nil {
		return fmt.Errorf("validate JSON schema: %w", err)
	}
	if result.Valid() {
		return nil
	}
	messages := make([]string, 0, len(result.Errors()))
	for _, item := range result.Errors() {
		messages = append(messages, item.String())
	}
	return errors.New(strings.Join(messages, "; "))
}

func executeSafely(ctx context.Context, tool BaseTool, arguments json.RawMessage) (output string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &Error{Code: "tool_panic", Message: fmt.Sprint(recovered), Fatal: true}
		}
	}()
	return tool.Execute(ctx, arguments)
}

func errorResult(callID, code, message string, retryable, fatal bool) schema.ToolResult {
	return schema.ToolResult{
		ToolCallID: callID,
		Output:     fmt.Sprintf("Error [%s]: %s", code, message),
		IsError:    true,
		ErrorCode:  code,
		Retryable:  retryable,
		Fatal:      fatal,
	}
}

func contextResult(callID string, err error) schema.ToolResult {
	code := "cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
	}
	return errorResult(callID, code, err.Error(), false, true)
}
