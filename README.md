# 01agent

`01agent` is a small, inspectable Agent Harness written in Go. It treats the
model as the planner and keeps reliability in code: a bounded ReAct loop,
provider adapters, deterministic tool dispatch, durable traces and checkpoints,
evaluation gates, permission checks, and an explicit workspace boundary.

The current release implements the supplied Agent Harness material through
lesson 13:

- a provider-neutral message and tool-call schema;
- an OpenAI-compatible provider and a Claude-compatible provider;
- an optional two-stage Thinking/Action loop;
- hard exits for turn, token, timeout, cancellation, permission, and fatal
  tool failures;
- a concurrency-aware tool registry with JSON Schema validation;
- a four-level, unique-only Edit fallback with stale-content SHA-256 checks;
- workspace-confined `read_file`, `write_file`, and `edit_file` tools plus an
  explicitly approved, platform-sandboxed Bash tool;
- revisioned capability snapshots and post-approval Turn execution leases;
- bounded read-only Subagents with fresh context and trace lineage;
- a canonical history writer with operation identity, semantic fingerprints,
  revision CAS, durable commit barriers, and read-back acknowledgement;
- JSONL traces, checkpoint/resume, deterministic replay, and context compaction;
- durable multi-Run sessions with per-session serialization, pending-turn
  recovery, and operation-ID replay;
- revisioned prompt snapshots with workspace `AGENTS.md`, lazy Skills, and
  `read_skill`;
- a raw context archive, tiered compaction, source IDs, and session-scoped
  `recall_context` search;
- canonical Plan state with revision CAS and repairable PLAN.md/TODO.md
  projections;
- an explicit Plan Mode switch for long-lived tasks, independent of the
  per-turn Thinking phase;
- a durable Feishu worker queue using the official Channel/WebSocket SDK;
- durable claim/ack inputs for user steering, tool input, and child-task results;
- a restart-safe background-task state machine with heartbeat, lost/reconcile,
  result delivery, and consumption acknowledgement;
- a distinct product Task v2 control plane with revision-CAS claims, immutable
  Artifacts, structured Handoffs, parent/child barriers, and evidence-bound Gates;
- a durable Agent Inbox with priority scheduling, per-Agent execution leases,
  coalesced conversation delivery, persistent work marks, and an atomic
  `read_seq` freshness barrier that retains stale replies as drafts;
- stable Agent identities with immutable configuration and Relationship
  revisions plus explicit Session generations, retirement, and continuity
  Handoffs;
- registered Computers with revisioned capability snapshots, outbound-only
  Daemon WebSockets, run-lease-safe Agent rebinding, and tracked old-device
  cleanup;
- retry/backoff, rate-spacing, and concurrency control around model providers;
- a 30-case runtime evaluator plus a 15-case Task v2 state-machine evaluator,
  both used as required CI gates;
- a reproducible CLI, tests, container image, CI, and GHCR publishing.

## Quick start

Requirements: Go 1.25 or newer and an API endpoint that implements either the
OpenAI Chat Completions protocol or the Anthropic Messages protocol.

```bash
go build -o bin/01agent ./cmd/01agent

export AGENT_PROVIDER=openai
export AGENT_API_KEY=your-key
export AGENT_MODEL=your-model
# Optional for OpenAI-compatible services such as Zhipu:
export AGENT_BASE_URL=https://open.bigmodel.cn/api/paas/v4/

./bin/01agent --workdir . "Read README.md and summarize the architecture."
```

Each run writes a JSONL trace, canonical history, operation ledger, inbox, and
final result under
`$AGENT_RUN_DIR` (or the platform temporary directory by default). Resume an
interrupted run with `--resume <run-id>` and validate a completed trace with:

```bash
go run ./cmd/01agent-replay --trace /path/to/run-id.trace.jsonl
```

Claude-compatible endpoints use `AGENT_PROVIDER=claude`. When
`AGENT_BASE_URL` is omitted, each official SDK uses its default endpoint.

Use `--thinking` for a separate tool-free planning call before each action
call. Thinking text is kept internal unless `--show-thinking` is explicitly
set.

Use `--plan-mode` for a long-lived task. It instructs the model to bootstrap or
resume the canonical external plan and to checkpoint each completed or blocked
step. This is independent from `--thinking`: Plan Mode is macro-level progress;
Thinking is local decision quality.

```bash
./bin/01agent --help
```

## Safety defaults

- The loop stops after 32 turns and after 10 minutes unless configured.
- Tool arguments are validated before permission checks and execution.
- Workspace writes and command execution are disabled by default. Read-only
  tools and harness-internal Plan state remain available.
- File tools use rooted filesystem handles to reject absolute paths, traversal,
  symlink escapes, and FIFO blocking/TOCTOU path swaps.
- Output is paginated instead of silently hiding the remainder of a file.
- Repeated identical calls are stopped before they become a doom loop.
- Provider retries apply only to transient failures and honor `Retry-After`.
- Write/edit/Bash require both startup enablement and approval. Unapproved
  exact calls create durable, expiring requests under the run directory.
- Approval is followed by a Turn-lease check immediately before physical tool
  execution, preventing a late approval from reviving a canceled Turn.
- Bash fails closed unless a platform sandbox is available. Linux uses Landlock
  plus seccomp; macOS uses `sandbox-exec`. Both deny networking and restrict
  writes to the workspace.

To make dangerous tools available to the model in the CLI, opt in twice:

```bash
./bin/01agent --enable-dangerous-tools \
  --approve-tool write_file --approve-tool edit_file \
  --workdir . "Update the requested files."
```

The Bash sandbox is defense in depth, not a replacement for least-privileged
deployment. Do not expose host credentials, Docker sockets, or unrelated data
to the service container.

See [the architecture](docs/architecture.md) and
[the course reading notes](docs/reading-notes.md) for design rationale.

## Verification

```bash
make verify
```

`make verify` formats-checks, vets, runs race-enabled tests, builds all eight
binaries, and executes the 30-case deterministic runtime suite plus the
15-case Task v2 state-machine suite. The runtime evaluator
records success rate, tool-sequence correctness, terminal reason, turns, token
usage, latency, and estimated cost. Its report and per-case traces are written
to `artifacts/eval/`; the configured 100% gate makes CI fail on any regression.

## Container

```bash
docker build -t 01agent:local .
docker run --rm \
  -e AGENT_PROVIDER -e AGENT_API_KEY -e AGENT_MODEL -e AGENT_BASE_URL \
  -v "$PWD:/workspace:ro" \
  01agent:local --workdir /workspace "Summarize README.md"
```

If `proxy.golang.org` is not reachable from the Docker daemon, pass a reachable
Go module proxy with `--build-arg GOPROXY=<url>,direct`.

Pushes to `main` publish `ghcr.io/royal007a/01agent:main`; version tags also
publish a matching immutable image tag.

## HTTP service

The same image includes `01agentd` for network deployment. Health is public;
executions require a bearer token. `/readyz` performs a real, cached provider
probe rather than reporting configuration presence as readiness. If model
variables are missing or the probe fails, it and `/v1/runs` return `503`.

```bash
export AGENT_API_TOKEN="$(openssl rand -hex 32)"
docker run -d --name 01agent-http -p 8080:8080 \
  -e AGENT_API_TOKEN -e AGENT_PROVIDER -e AGENT_API_KEY \
  -e AGENT_MODEL -e AGENT_BASE_URL \
  -e AGENT_RUN_DIR=/var/lib/01agent/runs \
  -v 01agent-runs:/var/lib/01agent/runs \
  --entrypoint /usr/local/bin/01agentd \
  01agent:local

curl http://127.0.0.1:8080/healthz
curl -H "Authorization: Bearer $AGENT_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Summarize README.md","plan_mode":false}' \
  http://127.0.0.1:8080/v1/runs
```

Resume a saved checkpoint with `{"resume_run_id":"run-..."}`. When dangerous
tools are enabled at server startup, an authenticated operator may preapprove
tool names for compatibility, for example
`{"prompt":"...","approved_tools":["write_file"]}`. Without preapproval the
response includes an `approval_id`. Inspect it with `GET /v1/approvals/{id}`,
decide it with `POST /v1/approvals/{id}/decision` using
`{"decision":"approved","actor":"operator"}`, then resume the run. Decisions
are exact-call, expiring, durable, and single-use.
The decision is also queued as durable tool input, so a resumed run observes it
through the normal claim/commit/ack barrier before deciding whether to retry.

For a durable multi-turn conversation, use the Session endpoint. The
`operation_id` should be the upstream message/event ID; retrying the same ID
with the same prompt returns the original Run result without invoking the
model again:

```bash
curl -H "Authorization: Bearer $AGENT_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"operation_id":"message-001","prompt":"Remember that the codename is Orion.","plan_mode":true}' \
  http://127.0.0.1:8080/v1/sessions/chat-001/turns

curl -H "Authorization: Bearer $AGENT_API_TOKEN" \
  http://127.0.0.1:8080/v1/sessions/chat-001
```

Set `AGENT_PLAN_MODE=true` to make Plan Mode the HTTP default; an individual Run
or Session request can override it with `plan_mode`. Keep it off for simple
lookups and short one-step actions.

A caller can choose a stable run ID and steer it while it is active. Inputs are
claimed, committed into canonical history, and acknowledged only after the
commit barrier succeeds:

```bash
curl -H "Authorization: Bearer $AGENT_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"steer-1","kind":"user_steer","content":"Also inspect deployment.md"}' \
  http://127.0.0.1:8080/v1/runs/run-demo/inputs
```

Background workers use `POST /v1/tasks`, `GET /v1/tasks/{taskID}`, and
`POST /v1/tasks/{taskID}/events`. Terminal task output is delivered to the
parent run as a `task_result` input. The daemon reconciles stale heartbeats to
`lost` and repairs delivery/consumption state after restart.

Product delivery Tasks are deliberately separate from those execution Jobs.
They use `POST/GET /v2/tasks`, `POST /v2/tasks/{taskID}/actions`, and
`POST /v2/tasks/{taskID}/artifacts`. Every mutation carries an idempotent
`operation_id` and `expected_revision`. A worker must hold the active claim
lease to attach an immutable Artifact and submit a Handoff. The configured
Gate checks the exact submitted Artifact versions; `pass`, `reject`, and
`needs_human` transition to `done`, `in_progress`, and `in_review`
respectively. Open child Tasks prevent their parent from entering review.

Agent attention uses `POST /v2/conversations/{conversationID}/messages` to
append a sequenced message and route it to Agent inboxes. An Agent claims one
item through `POST /v2/agents/{agentID}/inbox/claim`, then either acknowledges
or refreshes it through `POST /v2/inbox/{itemID}/actions`. Replies go through
`POST /v2/inbox/{itemID}/fresh-replies`: checking `read_seq`, appending the
reply, and acknowledging the inbox item happen under one durable transaction.
If new messages arrived, the API returns `409`, the missed messages, and a
persisted stale draft; the caller must refresh and deliberately revise or
confirm its reply. Work marks have separate create/clear endpoints and are not
cleared merely because an item became read or acknowledged.

Persistent Agent identities use `POST/GET /v2/agents`. Relationship changes
append a new immutable revision through
`POST /v2/agents/{agentID}/relationships/revisions`; older role and delegation
contracts remain inspectable. Session rotation is explicit through
`POST /v2/agents/{agentID}/sessions/rotate`: it retires exactly the expected
active generation with a structured continuity Handoff and creates one new
active Session generation. Agent identity, revisions, and work ownership
therefore survive Session replacement.

Computers are registered through `POST /v2/computers`. `01agent-daemon` then
opens an authenticated outbound WebSocket to `/v2/daemon/connect`, publishes a
digest-backed OS/architecture/runtime/tool/sandbox snapshot, and renews a
short connection lease while polling. Agent binding uses
`POST /v2/agents/{agentID}/computer-binding`; the server rejects offline
targets, stale binding revisions, and Agents with active run leases. A
successful move queues `cleanup_agent` for the old Computer. The Daemon removes
only `<daemon-root>/agents/<agentID>` through a rename-to-trash step and reports
`acked` or `failed`; cleanup failure is visible but never rolls back the new
binding. Credentials and paths outside the Daemon-managed root are untouched.

## Feishu bridge

`01agent-feishu` connects with the official Feishu Channel/WebSocket SDK. It
durably records each normalized message before acknowledging the callback,
then bounded workers call the Session API and reply in the original chat.
Feishu `event_id` is the idempotency key; interrupted jobs are reconciled after
restart.

```bash
export FEISHU_APP_ID=cli_xxx
export FEISHU_APP_SECRET=xxx
export AGENT_API_URL=http://127.0.0.1:8080
export AGENT_API_TOKEN=xxx
export FEISHU_QUEUE_DIR=/var/lib/01agent/feishu

/usr/local/bin/01agent-feishu
```

The SDK's default policy requires a bot mention in group chats, blocks
`@all`, and permits direct messages. `FEISHU_WORKERS`, `FEISHU_QUEUE_SIZE`,
`FEISHU_MAX_ATTEMPTS`, and `FEISHU_JOB_TIMEOUT` tune the durable worker pool.
`FEISHU_APPROVED_TOOLS` is an optional comma-separated list forwarded to each
Session turn; leave it empty unless the IM bot is intentionally allowed to use
dangerous tools.
