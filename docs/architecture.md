# Architecture

```text
CLI / HTTP host / durable Feishu bridge
       |
       v
SessionStore -- ordered Runs, operation idempotency, per-session lane
       |
       v
Prompt snapshot --> AgentEngine -- bounded loop, compaction, Turn lease, events
 AGENTS/Skills       |     |                                      |
                     |     +--> Resilient Provider                +--> RunStore
                     |          (retry/backoff/rate limits)             (canonical history/trace)
                     |                                                    |             |
                     |                                                    v             v
                     |                                               claim/ack inbox   TaskStore
                     |
                     +--> Memory archive <---- tiered compactor
                     |       |
                     |       +--> recall_context (session-scoped BM25)
                     |
                     +--> PlanStore --> plan.json + PLAN.md/TODO.md
                     |
                     +--> Registry -- validate, authorize, schedule, execute
                                          |
                                          +--> read_file / read_skill / recall_context
                                          +--> read_plan / update_plan
                                          +--> write_file / edit_file / bash (double-gated)
                                          +--> spawn_subagent -> isolated read-only AgentEngine
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
Recoverable tool failures receive guidance selected from their stable domain
error code. An equivalent call at the configured repeat limit emits a recent
soft reminder; one more equivalent request is rejected by the hard loop guard.

`SessionStore` is the ConversationManager above the query loop. It serializes
one session without blocking unrelated sessions, records a pending operation
before execution, and commits the new conversation only after a separate Run
has a durable result. Operation IDs make delivery idempotent; a restart can
finish the session barrier from a result or resume the Run checkpoint.

`PromptComposer` freezes the base prompt, workspace `AGENTS.md`, and Skill
catalog for one Run. Skill bodies are available only through `read_skill` and
are served from the pinned snapshot. Tool, prompt, AGENTS, and Skill digests
form the capability revision used by checkpoint compatibility and replay.

`Provider` owns wire translation. A wrapper retries only transient transport,
408/409/429, and 5xx failures, uses bounded exponential backoff, honors
`Retry-After`, and enforces request spacing and concurrency limits.

`Registry` owns the deterministic path from a model's requested action to a
tool result. It validates and authorizes before execution, then revalidates the
Turn lease immediately before invoking the physical tool. A batch is parallel
only when every requested tool is explicitly marked parallel-safe; observations
are returned in the model's original order.

`ApprovalStore` persists exact-call requests for dangerous actions. A request
binds the run, origin Turn lease, capability digest, tool, canonical arguments
digest, and expiry. The authenticated decision API can approve or reject it;
an approval is consumed exactly once when the same run requests the same call
again, and the active admission's Turn lease is then checked before execution.

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

`spawn_subagent` creates a fresh bounded child query loop for complex read-only
exploration. Its registry is rebuilt from an allowlist of `read` tools and never
contains Bash, mutations, external actions, or itself. Child runs inherit
cancellation and the parent's memory scope, carry explicit parent run/Turn
lineage in results and traces, and return only a size-bounded summary. A shared
semaphore bounds concurrent children.

`ContextCompactor` is injected into the loop. The default window compactor
first archives raw messages with stable source IDs. It then masks old tool
results, collapses old assistant prose, head/tail truncates large recent tool
observations, and retains recent complete tool-call/result groups. A
session-scoped lexical index powers `recall_context`; returned excerpts are
bounded so recall cannot immediately overflow the next model request.

`PlanStore` is separate from background task execution. It owns a model-facing
multi-step plan with operation identity, semantic fingerprint, revision CAS,
atomic commit, read-back verification, and repairable `PLAN.md`/`TODO.md`
projections. The explicit Plan Mode switch changes the snapshotted system
prompt, so resuming a checkpoint under a different mode fails capability
validation rather than silently changing its control policy.

The Feishu bridge uses the official Channel/WebSocket layer but keeps IM I/O
outside the engine. The callback durably enqueues and returns; bounded workers
call the HTTP Session API. Event IDs deduplicate delivery and persisted job
states are reconciled after restart.

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

`evals/runtime.json` contains 28 deterministic tasks covering direct answers,
single/paginated/parallel reads, recovery paths, loop and budget exits,
thinking/action separation, compaction, write/edit, Bash, approval denial,
canonical commits, queued user/task inputs, fuzzy edits, session continuity,
lazy Skills, archived recall, persistent Plan state, isolated Subagents, recovery hints,
soft-before-hard repeat intervention, and durable approval creation.
Each case writes and replays its real runtime trace before it is scored. The
gate currently requires all cases to pass.

The evaluator is a runtime conformance suite, not a claim about model quality.
Its scripted provider makes regressions reproducible and cost-free. A live-model
benchmark can reuse the same trace, result, and metric types without weakening
the deterministic CI gate.

## Dangerous tools

`write_file`, `edit_file`, and `bash` are absent unless explicitly enabled.
Even then, an unapproved exact call becomes a durable pending request. The
authenticated API may also supply explicit operator preapprovals for backward
compatibility. File changes are rooted and atomic. Bash has bounded time and
output, receives a minimal environment, and fails closed without its platform
sandbox. Linux uses Landlock ABI V4 for filesystem access and seccomp to deny
socket syscalls; macOS uses `sandbox-exec`. Container/user isolation remains an
additional required boundary.
