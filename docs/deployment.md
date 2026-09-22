# Deployment

The deployment image ships the one-shot `01agent` CLI, the `01agentd` HTTP
service, the `01agent-feishu` bridge, the evaluator/replay utilities, and the
Linux sandbox helper in one reproducible container.

## Automated GHCR publication

The `container.yml` workflow publishes multi-architecture images on every push
to `main` and for tags matching `v*`:

```text
ghcr.io/royal007a/01agent:main
ghcr.io/royal007a/01agent:vX.Y.Z
```

The workflow uses GitHub's short-lived repository token with only
`contents: read` and `packages: write`. No model credential is used while
building or publishing the image.

## Runtime

Supply provider credentials only when running the image. The safe default
exposes only `read_file`, so mount the workspace read-only:

```bash
docker run --rm \
  -e AGENT_PROVIDER=openai \
  -e AGENT_API_KEY \
  -e AGENT_MODEL \
  -e AGENT_BASE_URL \
  -v "$PWD:/workspace:ro" \
  ghcr.io/royal007a/01agent:main \
  --workdir /workspace "Inspect the repository and summarize its architecture."
```

For the official provider endpoints, omit `AGENT_BASE_URL`. For a compatible
endpoint, set it to the provider's API base URL.

## HTTP runtime

Run `/usr/local/bin/01agentd` as the container entrypoint and publish port 8080.
`GET /healthz` is unauthenticated for load balancers. `GET /readyz` makes a
real provider request and caches the result for `AGENT_READINESS_TTL` (five
minutes by default). `POST /v1/runs` requires
`Authorization: Bearer $AGENT_API_TOKEN` and a JSON body such as
`{"prompt":"Summarize README.md"}`.

The same listener serves a dependency-free management console at `GET /`.
Because it uses relative URLs, a reverse proxy may expose the complete service
under a prefix such as `/01agent/` as long as that prefix is stripped before
proxying. The page can load without credentials, but it cannot read or mutate
control-plane data until the operator supplies the bearer token; that value is
kept only for the current browser session. If the endpoint is deliberately
served over plain HTTP, restrict it to a trusted network because the bearer
token is not protected in transit.

The service limits request bodies, concurrent runs, total run time, turns,
repeated calls, provider traffic, and tool duration. Persist `AGENT_RUN_DIR` so
traces, canonical history, session turns, raw context archives, plan/TODO state,
inboxes, and background-task state survive container replacement:

```bash
docker volume create 01agent-runs
docker run -d --name 01agent-http -p 8080:8080 \
  --env-file .env \
  -v "$PWD:/workspace:ro" \
  -v 01agent-runs:/var/lib/01agent/runs \
  --entrypoint /usr/local/bin/01agentd \
  ghcr.io/royal007a/01agent:main
```

To resume, POST `{"resume_run_id":"run-..."}`. The checkpoint includes its
workspace path and the complete capability revision; resume rejects a workspace,
tool, prompt, AGENTS.md, or Skill snapshot mismatch and rejects already completed
runs.

Durable conversations use `POST /v1/sessions/{sessionID}/turns`. Supply a stable
`operation_id` in the body so a caller can retry without running the model or a
tool twice. The turn is persisted before execution, same-session turns are
serialized, and incomplete turns recover from a committed result or checkpoint.
`GET /v1/sessions/{sessionID}` returns the canonical conversation state.
Set `AGENT_PLAN_MODE=true` to enable external planning by default, or send a
per-request `plan_mode` boolean. Plan Mode is best reserved for long-lived work;
it is independent from `AGENT_THINKING`.

Context compaction archives omitted raw messages under `AGENT_RUN_DIR` before
shrinking the model-visible window. `recall_context` searches only the current
session/run archive, while `read_plan` and `update_plan` expose revision-checked
structured planning state. These stores are part of the durable runtime volume;
do not put them on ephemeral container storage.

The daemon reconciles background tasks every `AGENT_TASK_RECONCILE_INTERVAL`
(30 seconds by default). Running/stopping tasks whose heartbeat is older than
`AGENT_TASK_HEARTBEAT_TIMEOUT` (two minutes by default) become `lost`; pending
terminal delivery and parent-run consumption acknowledgements are also repaired.

Product Automations are evaluated every `AGENT_AUTOMATION_TICK_INTERVAL` (five
seconds by default). Schedule definitions, next-run timestamps, overlap skips,
failure counters, and run history live below the persistent `AGENT_RUN_DIR`.
Each dispatch creates a Product Task v2; a prior open Task prevents overlap,
and repeated reported failures pause the schedule until an authenticated status
operation resumes it.

## Computer daemon

Register the execution device through authenticated `POST /v2/computers`, then
run the bundled outbound client:

```bash
export AGENT_SERVER_URL=https://agent.example.com
export AGENT_API_TOKEN=xxx
export AGENT_COMPUTER_ID=builder-1
export AGENT_DAEMON_ROOT=/var/lib/01agent-daemon
/usr/local/bin/01agent-daemon
```

The Server needs no inbound route to the device. The client opens
`/v2/daemon/connect`, reports a revisioned capability snapshot, and maintains a
short lease while polling for typed commands. The root must be dedicated to the
Daemon. Old-device cleanup is limited to
`$AGENT_DAEMON_ROOT/agents/<agent-id>`; code, credentials, Provider sessions,
and data outside that root are not migrated or deleted. Configure the reverse
proxy to preserve WebSocket upgrades for this endpoint.

## Feishu bridge

Run the HTTP service and bridge against the same durable deployment. The bridge
uses the official Feishu Channel/WebSocket SDK and persists every event before
acknowledging it to the in-process worker queue. A Feishu `event_id` becomes the
idempotent operation ID sent to the Session API, so retries and process restarts
do not create duplicate Agent turns.

```bash
docker run -d --name 01agent-feishu \
  --env-file .env.feishu \
  -v 01agent-runs:/var/lib/01agent/runs \
  --entrypoint /usr/local/bin/01agent-feishu \
  ghcr.io/royal007a/01agent:main
```

Required bridge variables are `FEISHU_APP_ID`, `FEISHU_APP_SECRET`,
`AGENT_API_URL`, and `AGENT_API_TOKEN`. The default queue location is beneath
`AGENT_RUN_DIR`; override it with `FEISHU_QUEUE_DIR` only when that path is also
durable. Tune bounded concurrency with `FEISHU_WORKERS`, `FEISHU_QUEUE_SIZE`,
`FEISHU_MAX_ATTEMPTS`, and `FEISHU_JOB_TIMEOUT`. Use
`FEISHU_APPROVED_TOOLS` to pass an explicit comma-separated approval list to
each turn; leaving it unset keeps the safe read-only defaults.

## Dangerous-tool deployment

Set `AGENT_ENABLE_DANGEROUS_TOOLS=true` only inside a disposable,
least-privileged execution boundary and change the workspace mount to writable.
Registration alone is insufficient: every HTTP run must also list the exact
tools in `approved_tools`. Bash receives a minimal environment and bounded
timeout/output. It invokes `/usr/local/bin/01agent-sandbox`, which requires
Linux Landlock ABI V4 (Linux 6.8 satisfies this) and installs a seccomp filter
that denies socket syscalls. Startup fails if the helper is unavailable, and
execution fails closed if the kernel cannot install the policy. Do not mount
Docker sockets, credentials, host roots, or unrelated data into that container.

## Rollback

Use an immutable version tag or image digest in production. Rolling back is a
runtime configuration change to the previous tag/digest; it does not require
rewriting Git history.
