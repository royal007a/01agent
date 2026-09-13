# Deployment

`01agent` is a one-shot CLI rather than a long-running HTTP service. Deployment
therefore means publishing a reproducible executable container that operators
run with a prompt, credentials, and a mounted workspace.

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

Supply provider credentials only when running the image. Mount the workspace
read-only because this release exposes only `read_file`:

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

## Rollback

Use an immutable version tag or image digest in production. Rolling back is a
runtime configuration change to the previous tag/digest; it does not require
rewriting Git history.
