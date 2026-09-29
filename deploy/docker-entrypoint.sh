#!/bin/bash
# 容器入口：工作区（卷）首次启动时锁定环境，之后每次启动先比对——
# 镜像升级换了工具/依赖、或模型配置变了，都会在这里暴露出来（strict 模式直接拒绝启动）。
set -euo pipefail
if [ "${1:-}" = "serve" ]; then
  wd=/workspace
  if [ -f "$wd/.claw/env.lock.json" ]; then
    claw env check -dir "$wd" || [ "$(sed -n 's/.*"mode": *"\([a-z]*\)".*/\1/p' "$wd/.claw/env.lock.json")" != "strict" ]
  else
    claw env lock -dir "$wd"
  fi
fi
exec claw "$@"
