# Reading notes: the first six Agent Harness lessons

These notes summarize the six substantive PDFs (the opening essay plus lessons
1-5). They turn the material into implementation requirements instead of
copying the course examples verbatim.

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
