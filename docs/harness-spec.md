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
- An approval decision MUST be durably delivered to the paused run through the
  input claim/commit/ack protocol before a resumed model call observes it.

## Subagents

- A child MUST start with a fresh message timeline and explicit parent lineage.
- Its registry MUST include only `read` capabilities and MUST exclude the
  Subagent tool itself, Bash, state mutation, writes, and external actions.
- A child MUST have independent turn, token, output, and deadline bounds and
  MUST inherit parent cancellation.
- Concurrent children MUST be globally bounded.
- The parent MUST receive only the bounded child result and identifiers, not
  the child's full transcript.

## Product tasks and delivery gates

- Product Tasks MUST be distinct from runtime execution Jobs. A Task owns the
  objective, versioned requirements, modification scope, stop conditions,
  assignee, delivery state, and review contract.
- Every Task mutation MUST carry an idempotent operation ID, a semantic
  fingerprint, and an expected canonical revision. Reusing an operation ID
  with different semantics or writing against a stale revision MUST fail.
- Claiming MUST be atomic and lease-bound. Artifact attachment and submission
  MUST revalidate the active owner and lease immediately before commit.
- Artifacts MUST be immutable by `(artifact_id, version)` and content digest.
  Handoffs MUST bind the current contract revision and enumerate the exact
  Artifact versions and evidence submitted for review.
- A parent MUST NOT enter review while a child is outside `done` or `closed`.
- A Gate result MUST come from the configured reviewer and bind the exact
  submitted Artifact versions. `pass`, `reject`, and `needs_human` MUST result
  in `done`, `in_progress`, and `in_review` respectively; history is append-only.
- Canonical Task and Artifact writes MUST be atomic, synced, and read back
  before acknowledgement.

## Agent attention and freshness

- Permission to read a conversation MUST remain separate from the decision to
  wake an Agent. Deliverable messages MUST enter a durable Agent-level Inbox.
- Human corrections MUST outrank direct requests, Task reviews, and ordinary
  subscriptions. Pending messages for the same Agent and conversation MUST
  coalesce without discarding the original sequenced messages.
- By default an Agent MUST hold at most one active Inbox execution lease.
  Expired leases MUST make the same work claimable after restart.
- Claiming an item MUST record the conversation `read_seq`. Read cursors,
  Inbox acknowledgement, persistent work marks, and product Task completion
  are distinct states; changing one MUST NOT imply the others.
- Sending a reply MUST atomically compare the supplied `read_seq` with the
  conversation's current sequence, append the reply, and acknowledge the
  Inbox item. When the sequence is stale, no reply may be published; the draft
  and newly arrived messages MUST be persisted and returned for re-reading.
- Refreshing after a stale result MUST advance the claim's read snapshot under
  the same lease. The revised or explicitly confirmed reply MUST pass the
  freshness check again.

## Gate

Every feature MUST have unit tests plus at least one deterministic evaluator
case. A release is deployable only after unit tests, race tests, vet, builds,
trace replay, and the configured evaluator pass-rate gate all succeed.
