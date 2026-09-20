# Agent Harness specification

This document is normative. “MUST” requirements are acceptance criteria for
the runtime and deterministic evaluator.

## Recovery and loop control

- A recoverable tool error MUST preserve `error_code`, `retryable`, and
  `fatal`, and MAY add advice selected only from the stable error code.
- Recovery advice and behavioral reminders MUST be represented as sequenced
  trace events and survive canonical checkpoint commits.
- Equivalent calls MUST use canonical tool-name/arguments fingerprints.
- The runtime MUST issue a soft reminder at the repeat threshold and MUST stop
  before physically executing another equivalent call.
- Max turns, deadline, cancellation, and token budget remain hard exits.

## Human approval

- Mutable, executable, or external actions MUST be disabled at startup unless
  the operator enables those capabilities.
- An unapproved dangerous call MUST produce a durable pending request bound to
  run, Turn lease, capability revision, tool name, canonical arguments digest,
  expiration, and single-use decision state.
- Approval, rejection, expiry, cancellation, and stale lease MUST fail closed.
- The registry MUST recheck both approval and Turn lease immediately before
  invoking the physical tool.
- HTTP and Feishu are presentation adapters; the approval store and policy
  MUST remain channel-independent and replayable.

## Subagents

- A child MUST start with a fresh message timeline and explicit parent lineage.
- Its registry MUST include only `read` capabilities and MUST exclude the
  Subagent tool itself, Bash, state mutation, writes, and external actions.
- A child MUST have independent turn, token, output, and deadline bounds and
  MUST inherit parent cancellation.
- Concurrent children MUST be globally bounded.
- The parent MUST receive only the bounded child result and identifiers, not
  the child's full transcript.

## Gate

Every feature MUST have unit tests plus at least one deterministic evaluator
case. A release is deployable only after unit tests, race tests, vet, builds,
trace replay, and the configured evaluator pass-rate gate all succeed.
