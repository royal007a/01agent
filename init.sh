#!/usr/bin/env bash
# 环境初始化（环境锁定第 1 步）：固定起点，锁定运行时与依赖，再校验 Agent 工作区的环境锁。
#
#   ./init.sh [workdir]            默认工作区 ~/.go-tiny-claw/workspace
#   CLAW_BIN=~/.local/bin/claw ./init.sh   用已安装的二进制做门禁（make deploy 用）
#
# 首次运行生成 <workdir>/.claw/env.lock.json；之后每次都与之比对，出现错误级漂移时失败退出。
# 确认是有意升级后：claw env lock -dir <workdir>（或 make env-lock）重新锁定。
set -euo pipefail
cd "$(dirname "$0")"
source scripts/runtime-env.sh

# 1. 运行时：Go 工具链必须与 go.mod 的 toolchain 行一致
want=$(awk '/^toolchain /{print $2}' go.mod)
export GOTOOLCHAIN="$want" # toolchain 行只是下限，显式固定才是锁定：本机版本不同时 go 会自动下载精确版本
have=$(go env GOVERSION)
if [ "$have" != "$want" ]; then
  echo "❌ Go 工具链 ${have} ≠ go.mod 锁定的 ${want}（自动下载失败？检查 GOPROXY 网络）" >&2
  exit 1
fi

# 2. 依赖：go.sum 校验，模块缓存被篡改或与锁不符即失败
go mod download
go mod verify >/dev/null
echo "✅ 工具链 ${have}，依赖与 go.sum 一致"

# 3. 构建（-trimpath 去掉本机路径，同一源码在不同机器产出一致）
BIN="${CLAW_BIN:-}"
if [ -z "$BIN" ]; then
  go build -trimpath -o claw ./cmd/claw
  BIN=./claw
fi

# 4. Agent 工作区环境锁：无锁则锁定，有锁则比对
WD="${1:-$HOME/.go-tiny-claw/workspace}"
mkdir -p "$WD"
if [ -f "$WD/.claw/env.lock.json" ]; then
  "$BIN" env check -dir "$WD"
else
  "$BIN" env lock -dir "$WD"
fi
