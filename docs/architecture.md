# Architecture

```text
CLI / HTTP host
       |
       v
AgentEngine -- bounded loop, compaction, checkpoints, structured events
  |     |                                      |
  |     +--> Resilient Provider                +--> RunStore
  |          (retry/backoff/rate limits)             (trace/result/checkpoint)
  |
  +--------> Registry -- validate, authorize, schedule, execute
                           |
                           +--> read_file
                           +--> write_file / edit_file / bash (double-gated)
                                                       |
Evaluator <--------------------- consumes replay-validated traces
```

## Ownership

`AgentEngine` owns per-run state: the message timeline, turn count, usage
budget, repeated-call fingerprints, checkpoints, and terminal reason. It does
not know vendor SDK types or tool argument shapes. Every state transition emits
a sequenced event with a run ID and timestamp.

`Provider` owns wire translation. A wrapper retries only transient transport,
408/409/429, and 5xx failures, uses bounded exponential backoff, honors
`Retry-After`, and enforces request spacing and concurrency limits.

`Registry` owns the deterministic path from a model's requested action to a
tool result. It validates and authorizes before execution. A batch is parallel
only when every requested tool is explicitly marked parallel-safe; observations
are returned in the model's original order.

`RunStore` appends each event and the terminal result to a JSONL trace. It
atomically replaces checkpoints and result files. Replay verifies monotonic
sequence numbers, run identity, and tool-call/result pairing before an
evaluator trusts the trace.

`ContextCompactor` is injected into the loop. The default window compactor
retains the system/user prefix and recent complete tool-call/result groups,
then inserts an explicit compaction notice. Checkpoints persist the compacted
timeline so a resumed run continues from the same durable state.

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
- `persistence_error`
- `approval_required`

This keeps normal control-flow exits inspectable without forcing callers to
parse log messages.

## Evaluation gate

`evals/runtime.json` contains 20 deterministic tasks covering direct answers,
single/paginated/parallel reads, recovery paths, loop and budget exits,
thinking/action separation, compaction, write/edit, Bash, and approval denial.
Each case writes and replays its real runtime trace before it is scored. The
gate currently requires all cases to pass.

The evaluator is a runtime conformance suite, not a claim about model quality.
Its scripted provider makes regressions reproducible and cost-free. A live-model
benchmark can reuse the same trace, result, and metric types without weakening
the deterministic CI gate.

## Dangerous tools

`write_file`, `edit_file`, and `bash` are absent unless explicitly enabled.
Even then, a run receives only the dangerous tool names supplied in its
approval context. File changes are rooted and atomic. Bash has bounded time and
output and receives a minimal environment, but it is not an OS sandbox; its
container or host account remains the security boundary.
