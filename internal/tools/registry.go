package tools

import (
	"context"
	"crypto/sha256"
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
	Runtime
	Register(tool BaseTool) error
	Snapshot() Runtime
}

// Runtime is an immutable view of the capabilities exposed to one accepted
// agent turn. Definitions and dispatch always use the same physical tool set.
type Runtime interface {
	Revision() CapabilityRevision
	GetAvailableTools() []schema.ToolDefinition
	Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
	ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult
}

type CapabilityRevision struct {
	Sequence     uint64 `json:"sequence"`
	Digest       string `json:"digest"`
	ToolDigest   string `json:"tool_digest,omitempty"`
	PromptDigest string `json:"prompt_digest,omitempty"`
	AgentsDigest string `json:"agents_digest,omitempty"`
	SkillsDigest string `json:"skills_digest,omitempty"`
}

func (r CapabilityRevision) Equivalent(other CapabilityRevision) bool {
	return r.Digest != "" && r.Digest == other.Digest
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
	revision    CapabilityRevision
}

type runtimeSnapshot struct {
	tools       map[string]BaseTool
	policy      PermissionPolicy
	maxParallel int
	toolTimeout time.Duration
	revision    CapabilityRevision
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
	r.revision.Digest = capabilityDigest(r.tools)
	r.revision.ToolDigest = r.revision.Digest
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
	r.revision.Sequence++
	r.revision.Digest = capabilityDigest(r.tools)
	r.revision.ToolDigest = r.revision.Digest
	return nil
}

func (r *registryImpl) Revision() CapabilityRevision {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

func (r *registryImpl) Snapshot() Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make(map[string]BaseTool, len(r.tools))
	for name, tool := range r.tools {
		items[name] = tool
	}
	return &runtimeSnapshot{
		tools: items, policy: r.policy, maxParallel: r.maxParallel,
		toolTimeout: r.toolTimeout, revision: r.revision,
	}
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

func (r *runtimeSnapshot) Revision() CapabilityRevision { return r.revision }

func (r *runtimeSnapshot) GetAvailableTools() []schema.ToolDefinition {
	definitions := make([]schema.ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, tool.Definition())
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

func (r *registryImpl) lookup(name string) (BaseTool, bool) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	return tool, ok
}

func (r *runtimeSnapshot) lookup(name string) (BaseTool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	return execute(ctx, call, r.lookup, r.policy, r.toolTimeout)
}

func (r *runtimeSnapshot) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	return execute(ctx, call, r.lookup, r.policy, r.toolTimeout)
}

func execute(ctx context.Context, call schema.ToolCall, lookup func(string) (BaseTool, bool), policy PermissionPolicy, toolTimeout time.Duration) schema.ToolResult {
	if err := ctx.Err(); err != nil {
		return contextResult(call.ID, err)
	}

	tool, exists := lookup(call.Name)
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

	decision := policy.CanUse(ctx, definition, call.Arguments)
	if !decision.Allowed {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "tool use denied by policy"
		}
		code := "permission_denied"
		if decision.RequiresApproval {
			code = "approval_required"
		}
		result := errorResult(call.ID, code, reason, false, true)
		result.ApprovalID = decision.ApprovalID
		return result
	}
	// Authorization may involve an external approval and outlive the Turn that
	// requested it. Recheck cancellation and the Turn lease after approval but
	// before invoking any physical side effect.
	if err := ctx.Err(); err != nil {
		return contextResult(call.ID, err)
	}
	if err := ValidateExecutionLease(ctx); err != nil {
		return errorResult(call.ID, "stale_lease", err.Error(), false, true)
	}

	toolCtx := ctx
	cancel := func() {}
	if toolTimeout > 0 {
		toolCtx, cancel = context.WithTimeout(ctx, toolTimeout)
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
	return executeBatch(ctx, calls, r.Execute, r.canRunInParallel, r.maxParallel)
}

func (r *runtimeSnapshot) ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult {
	return executeBatch(ctx, calls, r.Execute, r.canRunInParallel, r.maxParallel)
}

func executeBatch(ctx context.Context, calls []schema.ToolCall, executeOne func(context.Context, schema.ToolCall) schema.ToolResult, canParallel func([]schema.ToolCall) bool, maxParallel int) []schema.ToolResult {
	results := make([]schema.ToolResult, len(calls))
	if len(calls) == 0 {
		return results
	}
	if !canParallel(calls) {
		for i, call := range calls {
			results[i] = executeOne(ctx, call)
			if results[i].Fatal {
				for j := i + 1; j < len(calls); j++ {
					results[j] = errorResult(calls[j].ID, "batch_cancelled", "not executed after a fatal tool result", false, true)
				}
				break
			}
		}
		return results
	}

	semaphore := make(chan struct{}, maxParallel)
	var wait sync.WaitGroup
	for i, call := range calls {
		wait.Add(1)
		go func(index int, item schema.ToolCall) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
				results[index] = executeOne(ctx, item)
			case <-ctx.Done():
				results[index] = contextResult(item.ID, ctx.Err())
			}
		}(i, call)
	}
	wait.Wait()
	return results
}

func (r *registryImpl) canRunInParallel(calls []schema.ToolCall) bool {
	return canRunInParallel(calls, r.lookup, r.maxParallel)
}

func (r *runtimeSnapshot) canRunInParallel(calls []schema.ToolCall) bool {
	return canRunInParallel(calls, r.lookup, r.maxParallel)
}

func canRunInParallel(calls []schema.ToolCall, lookup func(string) (BaseTool, bool), maxParallel int) bool {
	if len(calls) < 2 || maxParallel < 2 {
		return false
	}
	for _, call := range calls {
		tool, exists := lookup(call.Name)
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

func capabilityDigest(items map[string]BaseTool) string {
	definitions := make([]schema.ToolDefinition, 0, len(items))
	for _, tool := range items {
		definitions = append(definitions, tool.Definition())
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	encoded, _ := json.Marshal(definitions)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
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
