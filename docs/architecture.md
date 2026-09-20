# Architecture

```text
CLI / HTTP host
       |
       v
AgentEngine -- bounded loop, compaction, Turn lease, structured events
  |     |                                      |
  |     +--> Resilient Provider                +--> RunStore
  |          (retry/backoff/rate limits)             (canonical history/trace)
  |                                                    |             |
  |                                                    v             v
  |                                               claim/ack inbox   TaskStore
  |
  +--------> Registry -- validate, authorize, schedule, execute
                           |
                           +--> read_file
                           +--> write_file / edit_file / bash (double-gated)
                                                              |
                                                              v
                                                    platform sandbox
                                                       |
Evaluator <--------------------- consumes replay-validated traces
```

## Ownership

`AgentEngine` owns per-run state: the message timeline, Turn/admission identity,
usage budget, repeated-call fingerprints, checkpoints, input claims, and
terminal reason. At admission it freezes one immutable capability snapshot;
the provider definitions and physical dispatch use that same revision. Every
state transition emits a sequenced event with a run ID and timestamp.

`Provider` owns wire translation. A wrapper retries only transient transport,
408/409/429, and 5xx failures, uses bounded exponential backoff, honors
`Retry-After`, and enforces request spacing and concurrency limits.

`Registry` owns the deterministic path from a model's requested action to a
tool result. It validates and authorizes before execution, then revalidates the
Turn lease immediately before invoking the physical tool. A batch is parallel
only when every requested tool is explicitly marked parallel-safe; observations
are returned in the model's original order.

`RunStore` is the canonical history writer. Every mutation carries a monotonic
operation ID, semantic SHA-256 fingerprint, and expected revision. The writer
atomically replaces the checkpoint, `fsync`s it and its directory, reads it
back, then appends the operation ledger and returns an acknowledgement. Replay
verifies event order, run identity, frozen capability digest, commit revisions,
input claim/ack relations, and tool-call/result pairing before an evaluator
trusts the trace.

`InputQueue` persists user steering, additional tool input, and child-task
results. The engine claims a batch, includes it in a canonical history commit,
and acknowledges it only after the commit succeeds. On restart, committed
claims are acknowledged and uncommitted claims are released for redelivery.

`TaskStore` persists `queued`, `running`, `stopping`, `succeeded`, `failed`,
`canceled`, and `lost` states. Heartbeats establish executor liveness. A
terminal result is idempotently enqueued to its parent run, and is considered
consumed only after the parent input claim has a committed revision.

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

`evals/runtime.json` contains 23 deterministic tasks covering direct answers,
single/paginated/parallel reads, recovery paths, loop and budget exits,
thinking/action separation, compaction, write/edit, Bash, approval denial,
canonical commits, and queued user/task inputs.
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
output, receives a minimal environment, and fails closed without its platform
sandbox. Linux uses Landlock ABI V4 for filesystem access and seccomp to deny
socket syscalls; macOS uses `sandbox-exec`. Container/user isolation remains an
additional required boundary.
