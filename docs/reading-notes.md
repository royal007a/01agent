# Reading notes: Agent Harness lessons 0-17

These notes summarize the supplied course material through lesson 13. They
turn the material into implementation requirements instead of copying the
pedagogical examples verbatim.

## 0. Opening: framework collapse and the Harness model

The central analogy is an operating system. The LLM is the CPU, the context
window is scarce RAM, and tools are hardware devices. A Harness should avoid
micromanaging the model's business logic; it should enforce the physical laws
that make long-running work safe and observable.

Three recurring failure classes motivate the design:

1. Context loss: too many tool descriptions and oversized results dilute
   attention or exceed the model window.
2. State loss: hidden framework state makes long tasks opaque and allows
   repeated failed actions to become doom loops.
3. Boundary loss: a model with local tools can act outside the intended
   workspace unless code enforces the boundary before execution.

The proposed response is a minimal tool surface, state that can be inspected
outside process memory, and defense in depth around every physical action.

## 1. From Framework to Harness

Static DAG workflows are useful when the process is known in advance, but they
become brittle for open-ended engineering tasks. A Harness inverts control:
the model chooses the next action from current context, while code owns
validation, dispatch, compaction, permissions, and interruption.

The project map has four layers:

- Entry/UI: CLI first; asynchronous human approval can be added later.
- Core engine: the ReAct loop, provider boundary, and optional Thinking phase.
- Context engineering: prompt composition, token pressure, compaction, and
  file-backed task memory are future layers.
- Tool execution: a registry, a small set of primitives, and middleware that
  checks each action before it touches the system.

## 2. The Main Loop

The production mapping of ReAct is structured protocol traffic:

```text
model response -> zero tool calls -> final answer
               -> tool calls      -> validate/execute -> tool results -> repeat
```

The context transcript is the loop's only evolving state. Tool call IDs must
be preserved so every observation is associated with the model request that
created it. Multiple independent read operations can run concurrently, but
results must be appended in original call order. Mutating or conflicting tools
must remain serial.

The lesson starts with an unbounded loop for clarity. For production this
repository adds non-negotiable exits: max turns, deadline/cancellation, token
budget, permission denial, repeated-call detection, and fatal tool failures.

## 3. Optional Thinking phase

Models can call an available tool before they have grounded a plan. The lesson
introduces a two-call turn:

1. Thinking: call the model without structured tools, forcing a text plan.
2. Action: append the plan, restore tools, and let the model act.

This is deliberately optional. It trades latency and tokens for a clean
intervention point, and it can create anchoring when an early plan is wrong.
The CLI therefore defaults it off and hides the trace unless the operator asks
to see it. Native-reasoning models may not benefit from the extra phase.

## 4. Provider adapters

The engine must not know a vendor SDK. Providers translate the internal
message timeline and tool definitions to one wire protocol, then translate
assistant text, usage, and tool calls back into the internal schema.

The two required protocol families are:

- OpenAI-compatible Chat Completions, including assistant tool calls and tool
  result messages.
- Anthropic-compatible Messages, including `tool_use` and `tool_result`
  content blocks.

Both adapters accept a configurable base URL so a compatible service can be
used without branching inside the engine. Constructors return errors instead
of panicking when configuration is missing.

## 5. Tool Registry and `read_file`

The registry is both a catalog and an execution boundary. Every tool provides
a unique name, description, JSON Schema, risk classification, concurrency
policy, semantic validation, and execution function. Dispatch follows a fixed
order:

```text
lookup -> JSON Schema validation -> tool validation -> permission -> execute
```

Unknown tools and recoverable errors are returned to the model as structured
observations. Permission failures and explicitly fatal errors terminate the
loop. Definitions are sorted for reproducible prompts, and duplicate names are
rejected instead of silently changing behavior.

`read_file` is the first physical primitive. It accepts only paths under the
configured workspace, follows symlinks only when their resolved target stays
inside that workspace, rejects directories, honors cancellation, and returns
bounded byte ranges with a continuation offset. This improves on a hard 8 KB
cut: the model can request the next page instead of being permanently blind to
the rest of a large file.

## Implementation checklist

- [x] Provider-neutral schema
- [x] Bounded, cancellable ReAct loop
- [x] Optional Thinking/Action split
- [x] OpenAI-compatible adapter
- [x] Claude-compatible adapter
- [x] Validating, permission-aware registry
- [x] Ordered parallel execution for read-only tools
- [x] Safe paginated file reading
- [x] CLI and environment configuration
- [x] Unit, race, vet, and build checks
- [x] Container packaging and continuous deployment to GHCR

## 7. A tolerant but safe Edit tool

Exact replacement is the safest path, but model-generated `old_text` often
loses line-ending or indentation detail. The implemented fallback chain is:

```text
exact -> normalized newline -> outer blank lines -> line indentation
```

Every level still requires one unique match. A fuzzy `replace_all` is never
allowed. Unlike the lesson's compact example, the implementation maps a match
back to the original byte range, preserves CRLF/LF conventions, reindents a
replacement against the target block, serializes mutations of the same path,
supports an `expected_sha256` precondition, performs an atomic replace, and
verifies the written digest by reading it back.

## 8. Parallel tool calls

Fork/join must preserve result order, but the lesson's initial “parallelize
everything” independence assumption is too weak for production. 01agent
parallelizes a batch only when every tool is classified as read-only and
parallel-safe. Batches containing writes or execution remain serial, global
parallelism is bounded, and a fatal result cancels the remaining serial batch.

## 9. Feishu integration

The useful abstraction is not `fmt.Println` versus a Feishu sender; it is an
I/O boundary around the engine. The native bridge uses the official Channel
and WebSocket SDK for normalization, mention policy, sending, and transport
lifecycle. Its event callback does only a durable enqueue and returns quickly.
Workers call the HTTP Session API outside the callback, use `event_id` as an
idempotency key, map a chat to a stable session, and persist queued,
processing, succeeded, or failed delivery state. Restart reconciliation moves
unacknowledged processing jobs back to queued.

This deliberately avoids the lesson example's unbounded goroutine-per-message
pattern and shared-workspace race.

## 10. Prompt composition, AGENTS.md, and Skills

The system prompt is a versioned capability, not a constant string. At run
admission the composer snapshots the base prompt, workspace `AGENTS.md`, and
`.01agent/skills/*/SKILL.md`. Only skill name, description, and revision enter
the prompt. The body remains outside the context until the model calls
`read_skill`, which reads from that run's immutable snapshot rather than from
the live filesystem.

Tool, prompt, AGENTS, and Skill digests are combined into the capability
revision stored in checkpoints and traces. Resume fails if any component has
changed, making replay behavior explicit rather than silently mixing versions.

## 11. Session isolation and working memory

A Run is one query-loop execution; a Session is an ordered conversation made
of multiple Runs. The file-backed Session store maps stable session IDs to
messages and completed turns. Each submitted turn has an operation ID and a
separate run ID. Same-session turns are serialized while different sessions
can execute concurrently.

The store persists a pending turn before execution and commits the updated
conversation only after the Run result is durable. A duplicate operation ID
replays the original result. After a crash, recovery first looks for a
completed Run result, then a checkpoint, and only starts fresh when neither
exists.

## 12. Tiered context compaction

Compaction affects provider input, never the authoritative raw archive. Before
lossy transformation, messages are externalized with stable source IDs. The
compactor then masks old tool observations, collapses old assistant prose,
head/tail truncates unusually large recent observations, and finally drops a
middle window while preserving recent complete tool-call/result groups.

Markers instruct the model to use `recall_context`. The recall tool performs
session-scoped BM25-style lexical ranking, returns source IDs and bounded
excerpts, and never crosses session boundaries. Conflict precedence is stated
in the compacted context: newer raw evidence wins over older raw evidence,
which wins over derived summaries.

## 13. Externalized plan and durable memory

Long tasks need state that survives model calls and process restarts. 01agent
uses three distinct stores instead of treating all “memory” as one blob:

- canonical Run history and trace for protocol recovery and replay;
- a session-scoped raw archive for detail recall;
- a revision-CAS Plan store for model-maintained task progress.

`update_plan` creates or updates a structured plan with an operation ID,
semantic fingerprint, and expected revision. The canonical JSON is atomically
committed and read back before acknowledgement. `PLAN.md` and `TODO.md` are
human-readable projections that an idempotent retry can repair after a crash.
Internal state tools use the separate `state` risk class: they may mutate only
the harness state directory and do not gain workspace-write authority.

Plan Mode is explicit (`--plan-mode`, `AGENT_PLAN_MODE`, or per-request
`plan_mode`) and is intentionally independent from Thinking. Plan Mode provides
macro navigation across turns and restarts; Thinking remains a micro-level
reasoning pass. The production implementation stores per-session plans under
the durable Harness state directory rather than polluting a shared target
workspace. Markdown files are inspectable projections; canonical updates go
through the revision-checked tool so manual or concurrent writes cannot silently
win.

## Implementation status for lessons 7-13

- [x] Four-level, unique-only Edit fallback with stale-write detection
- [x] Bounded, ordered, read-only parallel tool scheduling
- [x] Durable Feishu queue and official WebSocket Channel bridge
- [x] Per-run AGENTS/Skill snapshot and lazy `read_skill`
- [x] Durable multi-Run Session manager and idempotent turn API
- [x] Tiered compaction, raw archive, source IDs, and `recall_context`
- [x] Revisioned Plan state plus PLAN.md/TODO.md projections

## 14. Context-aware error recovery

Tool errors are observations, but terse observations often cause a model to
retry blindly. Recovery advice belongs at the harness boundary and is keyed by
stable domain error codes such as `no_match`, `ambiguous_match`, and
`edit_conflict`; matching human-readable OS error strings is deliberately
avoided. The original structured fields remain authoritative, while the tool
observation and trace receive concise corrective guidance. Unknown retryable
errors get only generic advice so the engine does not invent domain policy.

## 15. Soft reminders and hard loop limits

Recent reminder messages can overcome recency bias, but they are probabilistic
behavior shaping rather than a safety boundary. Equivalent calls use the
canonical tool/arguments fingerprint already persisted in checkpoints. On the
configured limit, 01agent appends and traces one user-role system reminder only
after all tool-call/result pairs are complete. A subsequent equivalent request
hits the existing deterministic terminal guard; max turns, tokens, and deadline
remain independent hard limits.

## 16. Approval middleware

Dangerous capability admission needs three outcomes: allow, deny, or ask. An
approval must bind to the exact run, Turn lease, capability revision, tool,
arguments fingerprint, and expiration; approving only a tool name is too
broad. Pending decisions must survive restart, allow explicit rejection, and
be consumed once. Cancellation or a stale lease must still be checked directly
before execution. Channel adapters may present buttons or commands, but policy
and durable state remain channel-independent.

## 17. Context-isolated Subagents

A Subagent is a bounded child query loop with a fresh transcript, not another
message in the parent's growing context. It receives only a frozen read-only
tool snapshot, excludes the spawn tool to prevent recursion, inherits
cancellation, has its own turn/token/deadline limits, and returns a bounded
summary to the parent. Child run identity and parent lineage must be visible in
traces. Shell access is not considered read-only even when the prompt asks it
to behave, so Bash is never included in the child registry.

## Implementation status for lessons 14-17

- [x] Error-code-driven recovery guidance and trace events
- [x] Soft repeat reminder before the deterministic hard stop
- [x] Durable exact-call approval requests and decision API
- [ ] Bounded, read-only, recursion-free Subagent tool
