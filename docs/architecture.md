# Architecture

```text
CLI / host application
        |
        v
AgentEngine  -- owns one bounded task turn
  |     |
  |     +--> Provider -- translates internal messages to an LLM protocol
  |
  +--------> Registry -- validates, authorizes, schedules, and executes tools
                           |
                           +--> read_file (workspace-confined primitive)
```

## Ownership

`AgentEngine` owns transient per-run state: the message timeline, turn count,
usage budget, repeated-call fingerprints, and terminal reason. It does not know
vendor SDK types or tool argument shapes.

`Provider` owns wire translation. The internal schema keeps provider-specific
unions out of the engine and tools.

`Registry` owns the deterministic path from a model's requested action to a
tool result. It validates and authorizes before execution. A batch is parallel
only when every requested tool is explicitly marked parallel-safe; observations
are returned in the model's original order.

## Terminal reasons

Every run returns one explicit outcome:

- `completed`
- `max_turns`
- `aborted`
- `timeout`
- `permission_denied`
- `budget_exceeded`
- `fatal_tool_error`
- `provider_error`

This keeps normal control-flow exits inspectable without forcing callers to
parse log messages.

## Deliberate scope boundary

This repository implements only the material in the supplied six PDFs. Bash,
write/edit, context compaction, externalized PLAN/TODO state, human approval,
and tracing belong to later lessons and are not silently approximated here.
The interfaces leave room for those additions without changing the Main Loop.
