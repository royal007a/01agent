# Competitor review log

## 2026-09-23 — baseline pull

Reviewer: mymaccodex  
Repositories (shallow-cloned for this review):

- OpenClaw — <https://github.com/openclaw/openclaw> — `7b68b47ffd6cfe2ffc52342e19b6fd03ec32b609`
- OpenHands Software Agent SDK — <https://github.com/OpenHands/software-agent-sdk> — `d5dc4457b4a2aeded40edcfa909b97ed01cd69d0`
- Goose — <https://github.com/block/goose> — `3f47426e6af4681aacab735c781426aae58a3bac`

Commands/evidence:

- `git clone --depth 1` for all three repositories.
- OpenClaw `docs/agent-runtime-architecture.md`, `docs/concepts/memory-provenance.md`,
  and channel integration tests/docs.
- OpenHands SDK `README.md` and its `openhands-agent-server/`, clients, and tests.
- Goose `README.md`, `scripts/test_subagents.sh`, `scripts/test_compaction.sh`,
  `scripts/run-benchmarks.sh`, and deployment files.
- 01agent baseline: `README.md`, `internal/dispatcher/`, `internal/daemon/`,
  `internal/runstore/`, `internal/automation/`, `internal/eval/`, and
  `internal/server/web/`.

### Findings

Adopt:

- Keep OpenClaw's explicit runtime layering and source-linked memory provenance
  as comparison targets for our existing canonical history, compaction, and
  replay design.
- Keep OpenHands' SDK/server/client split as a target for a typed external
  client contract; our current control plane is primarily an embedded Go
  console/API.
- Keep Goose's repeatable shell scripts for subagent, compaction, and benchmark
  smoke tests as a model for operator-friendly reproducibility.

Missing or likely behind (requires a follow-up spike before calling a gap
confirmed):

- **External integrations:** OpenClaw has a much broader channel/plugin surface
  and channel-specific recovery/approval behavior. 01agent currently has the
  Feishu worker path and generic HTTP control plane; add an integration matrix
  and prioritize one second channel only after the Dispatcher gates remain
  green.
- **Client/server SDK surface:** OpenHands publishes REST/WebSocket contracts
  and typed clients around its Agent Server. 01agent exposes HTTP endpoints and
  an outbound Daemon protocol, but lacks a separately versioned client SDK and
  OpenAPI contract.  Prototype an exported schema/client before committing to
  compatibility promises.
- **Benchmark breadth:** 01agent's deterministic runtime/Task/Dispatcher
  evaluators cover harness correctness.  Goose and OpenHands ship more
  operator-facing smoke/benchmark tooling; we need a reproducible quality,
  latency, token, and cost matrix on real coding tasks to measure whether our
  harness is actually behind.
- **Memory lifecycle evidence:** OpenClaw documents provenance-aware memory
  ingestion and forgetting.  01agent has source IDs, raw archives, recall,
  and compaction, but does not yet have a user-facing provenance/forget report.
  Treat this as a privacy/operations spike, not an automatic feature copy.
- **Runtime breadth:** Goose and OpenHands cover more local/remote runtime and
  workspace packaging paths.  01agent's platform sandbox and Computer/Daemon
  deployment matrix is narrower; add platform-specific acceptance cases only
  where they improve a declared deployment target.

Rejected for now:

- Copying broad channel count or model/provider lists without a failure-mode,
  security, and maintenance evaluation.
- Treating repository size, stars, or README claims as evidence of superiority.

01agent follow-ups:

- Create a Task for a real-task benchmark matrix with quality/cost/latency
  gates; do not weaken current deterministic gates.
- Create a design Task for versioned external API/OpenAPI plus a generated
  client, with replay and auth compatibility tests.
- Create a privacy Task for provenance-aware memory inspection/forgetting,
  including a Gate proving source records and derived artifacts are handled
  consistently.
- Re-review after those spikes and record exact test results here.
