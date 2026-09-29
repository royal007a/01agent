# 固定的运行时环境：服务端（launchd）、init.sh、部署门禁都 source 这一份，
# 保证"锁定时的环境"和"运行时的环境"是同一个起点（交互 shell 的 PATH/函数/别名不算数）。
# 用法: source scripts/runtime-env.sh
_repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export LC_ALL="${LC_ALL:-en_US.UTF-8}"
source "$_repo/scripts/env.sh" "${CLAW_PROTO:-openai}"
if [ -f "$HOME/.go-tiny-claw/serve.env" ]; then
  set -a; source "$HOME/.go-tiny-claw/serve.env"; set +a
fi
unset _repo
