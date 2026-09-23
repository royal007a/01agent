#!/usr/bin/env bash
# launchd 入口：启动时才从 ~/.hermes/.env 读取模型 key（不把密钥写进 plist），
# 可选读取 ~/.go-tiny-claw/serve.env（FEISHU_APP_ID/SECRET、CLAW_HTTP_TOKEN、CLAW_PROVIDER 等覆盖项）。
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
source "$REPO/scripts/env.sh" "${CLAW_PROTO:-openai}"
if [ -f "$HOME/.go-tiny-claw/serve.env" ]; then
  set -a; source "$HOME/.go-tiny-claw/serve.env"; set +a
fi
exec "${CLAW_BIN:-$HOME/.local/bin/claw}" serve -addr "${CLAW_ADDR:-127.0.0.1:48080}" ${CLAW_SERVE_FLAGS:-}
