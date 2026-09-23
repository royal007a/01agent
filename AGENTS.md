# 01agent Engineering Instructions

## Competitive agent-platform review

This repository is an inspectable Agent Harness. In addition to implementing
features, regularly pull and review current open-source competitors so that
roadmap decisions are based on executable evidence rather than feature lists.

### Review cadence

- Run a lightweight review at least once per sprint, and before proposing a
  major control-plane, runtime, sandbox, or evaluation change.
- Review the upstream default branch at the time of review. Record the review
  date, commit SHA, repository URL, and the exact commands or tests run.
- Prefer shallow clones under `/tmp/01agent-competitor-review/<date>/<name>`;
  never vendor competitor code into this repository.
- Do not copy code or licenses into 01agent without an explicit license and
  dependency review.

### Baseline competitors

Use these as the initial comparison set. Add a project when it introduces a
materially different runtime, sandbox, evaluation, or multi-agent model.

- OpenClaw: https://github.com/openclaw/openclaw
- OpenHands Software Agent SDK: https://github.com/OpenHands/software-agent-sdk
- Goose: https://github.com/block/goose

The list is a starting point, not an endorsement. Verify repository activity,
license, and current architecture at every review.

### Pull and inspect

```bash
review_date=$(date -u +%Y%m%d)
review_root="/tmp/01agent-competitor-review/$review_date"
mkdir -p "$review_root"
git clone --depth 1 https://github.com/openclaw/openclaw "$review_root/openclaw"
git clone --depth 1 https://github.com/OpenHands/software-agent-sdk "$review_root/openhands-sdk"
git clone --depth 1 https://github.com/block/goose "$review_root/goose"

for repo in "$review_root"/*; do
  printf '\n== %s ==\n' "$repo"
  git -C "$repo" log -1 --format='%H %cI %s'
  git -C "$repo" diff --stat HEAD~1..HEAD 2>/dev/null || true
done
```

Read source and tests, not only README files. At minimum inspect:

1. run/turn lifecycle, durable state, restart and cancellation semantics;
2. provider abstraction, retries, rate limits, context compaction and replay;
3. tool registry, permissions, approvals, sandbox and workspace isolation;
4. task decomposition, multi-agent handoff, review gates and freshness;
5. trace schema, evaluator coverage, reproducibility, latency and cost;
6. daemon/remote execution, web/API surface and operational deployment.

### Evidence and comparison rules

For every claimed capability, capture one of:

- a source path and symbol;
- a test path and command with its result;
- a documented API contract plus a passing request; or
- `not found` after recording the search paths and terms used.

Classify findings as follows:

- **Adopt**: a concrete design improves reliability or operability and fits
  the 01agent invariants;
- **欠缺 / missing**: the capability is needed by our stated product goals and
  has no equivalent implementation here;
- **落后 / behind**: an equivalent exists but the competitor has stronger
  evidence, broader provider/runtime coverage, lower operational cost, or a
  materially better failure mode;
- **Reject**: the trade-off conflicts with least privilege, durable history,
  explicit ownership, or our evaluation gates.

Never label a project “better” from star count or marketing text alone. A
competitor claim becomes a roadmap item only after a small reproducible spike
or an evaluator case demonstrates the value.

### Review record

Append each review to `docs/competitor-reviews.md` (create it if absent) with:

```text
date:
reviewer:
repositories: name, URL, commit SHA
commands/tests:
adopt:
missing:
behind:
rejected:
01agent follow-ups: issue/Task IDs, owner, gate, due date
```

Keep the record factual and link each conclusion to evidence. Convert accepted
follow-ups into a Task with requirements, a Gate, and an evaluator case before
implementation. Do not silently change AGENTS.md to make a competitor result
look favorable; update this instruction only when the review process itself
changes.

### Current 01agent review lens

The current baseline already covers a durable ReAct harness, provider retries,
revisioned capabilities, approval leases, context compaction, canonical
history, Task v2/Handoff/Gate, Agent Inbox/work marks/freshness, Computer and
Daemon dispatch, Automation, Team Lockfiles, and deterministic evaluators.
The highest-risk comparison gaps to verify first are:

- breadth and maturity of external runtime/provider integrations;
- platform sandbox backends and deployment ergonomics beyond the current
  container plus host-specific sandbox paths;
- benchmark breadth beyond deterministic harness cases, including task-level
  quality, cost, latency, and regression comparability;
- interoperability with external tool/skill ecosystems and import/export;
- production observability, operator workflows, and multi-tenant isolation.

These are hypotheses for the next review, not claims that a competitor wins.
Each must be changed to evidence-backed `missing`, `behind`, `adopt`, or
`reject` in `docs/competitor-reviews.md`.
