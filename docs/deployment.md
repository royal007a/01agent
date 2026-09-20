# Deployment

The deployment image ships both the one-shot `01agent` CLI and the `01agentd`
HTTP service in one reproducible container.

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

The service limits request bodies, concurrent runs, total run time, turns,
repeated calls, provider traffic, and tool duration. Persist `AGENT_RUN_DIR` so
traces, canonical history, inboxes, and task state survive container replacement:

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
workspace path; resume rejects a mismatch and rejects already completed runs.

The daemon reconciles background tasks every `AGENT_TASK_RECONCILE_INTERVAL`
(30 seconds by default). Running/stopping tasks whose heartbeat is older than
`AGENT_TASK_HEARTBEAT_TIMEOUT` (two minutes by default) become `lost`; pending
terminal delivery and parent-run consumption acknowledgements are also repaired.

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
