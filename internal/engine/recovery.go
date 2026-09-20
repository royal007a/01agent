package engine

import (
	"fmt"
	"strings"

	"github.com/royal007a/01agent/internal/schema"
)

const reminderPrefix = "<system-reminder>"

// recoveryHint maps stable domain error codes to corrective guidance. The
// harness deliberately does not inspect provider- or OS-specific error text:
// tools own classification, while the engine owns model-facing recovery.
func recoveryHint(result schema.ToolResult) string {
	if !result.IsError || result.Fatal {
		return ""
	}
	switch result.ErrorCode {
	case "no_match":
		return "Read the current file again, then retry with exact text and enough surrounding context to identify one location."
	case "ambiguous_match":
		return "Read the relevant region again and include more unique surrounding context; do not guess which occurrence to edit."
	case "edit_conflict":
		return "The file changed after it was read. Re-read it, use the new digest, and recompute the edit from current contents."
	case "open_file", "stat_file", "read_file", "offset_out_of_range":
		return "Verify the path and current file metadata with available read-only tools, then retry using the observed state."
	case "schema_validation", "argument_validation", "invalid_arguments":
		return "Correct the tool arguments to match its schema and validation error before retrying."
	case "command_failed":
		return "Inspect the exit code and output, fix quoting or command assumptions, and retry only with a materially changed command."
	case "plan_not_found":
		return "Create the plan with expected_revision 0 before reading or updating it."
	case "plan_conflict":
		return "Read the latest plan revision, merge against that canonical state, and retry with a new operation_id."
	case "unknown_skill":
		return "Choose a skill from the pinned capability catalog; live filesystem additions are unavailable until a new run."
	case "memory_search":
		return "Narrow or rephrase the recall query and continue from recent raw context if no archived detail is available."
	default:
		if result.Retryable {
			return "Use the structured error code as evidence, change the approach or arguments, and avoid repeating the equivalent call."
		}
		return ""
	}
}

func attachRecoveryHint(result schema.ToolResult) (schema.ToolResult, string) {
	hint := recoveryHint(result)
	if hint == "" {
		return result, ""
	}
	result.Output = strings.TrimSpace(result.Output) + "\nRecovery guidance: " + hint
	return result, hint
}

func repeatedCallReminder(toolName string, count, limit int) schema.Message {
	return schema.Message{
		Role: schema.RoleUser,
		Content: fmt.Sprintf(
			"%s The equivalent %s call has been attempted %d times (hard limit %d). Stop repeating it. Reassess the latest observations, change the arguments or approach, or explain why progress is blocked. </system-reminder>",
			reminderPrefix, toolName, count, limit,
		),
	}
}
