# 01agent

`01agent` is a small, inspectable Agent Harness written in Go. It treats the
model as the planner and keeps reliability in code: a bounded ReAct loop,
provider adapters, deterministic tool dispatch, permission checks, and an
explicit workspace boundary.

The first release implements the material covered by the first six course
PDFs supplied for this project:

- a provider-neutral message and tool-call schema;
- an OpenAI-compatible provider and a Claude-compatible provider;
- an optional two-stage Thinking/Action loop;
- hard exits for turn, token, timeout, cancellation, permission, and fatal
  tool failures;
- a concurrency-aware tool registry with JSON Schema validation;
- a workspace-confined, paginated `read_file` tool;
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

Claude-compatible endpoints use `AGENT_PROVIDER=claude`. When
`AGENT_BASE_URL` is omitted, each official SDK uses its default endpoint.

Use `--thinking` for a separate tool-free planning call before each action
call. Thinking text is kept internal unless `--show-thinking` is explicitly
set.

```bash
./bin/01agent --help
```

## Safety defaults

- The loop stops after 32 turns and after 10 minutes unless configured.
- Tool arguments are validated before permission checks and execution.
- Only read-only tools are enabled in this release.
- `read_file` rejects absolute paths, traversal, and symlink escapes.
- Output is paginated instead of silently hiding the remainder of a file.
- Repeated identical calls are stopped before they become a doom loop.

See [the architecture](docs/architecture.md) and
[the six-PDF reading notes](docs/reading-notes.md) for design rationale.

## Verification

```bash
go test -race ./...
go vet ./...
go build ./cmd/01agent
```

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
executions require a bearer token. If model variables are missing, `/healthz`
still reports the process as live while `/readyz` and `/v1/runs` return `503`.

```bash
export AGENT_API_TOKEN="$(openssl rand -hex 32)"
docker run -d --name 01agent-http -p 8080:8080 \
  -e AGENT_API_TOKEN -e AGENT_PROVIDER -e AGENT_API_KEY \
  -e AGENT_MODEL -e AGENT_BASE_URL \
  --entrypoint /usr/local/bin/01agentd \
  01agent:local

curl http://127.0.0.1:8080/healthz
curl -H "Authorization: Bearer $AGENT_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Summarize README.md"}' \
  http://127.0.0.1:8080/v1/runs
```
