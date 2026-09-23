# 本机真实模型配置：从 ~/.hermes/.env 读取方舟（Volcengine Ark）Coding Plan key，不落盘复制。
# 用法: source scripts/env.sh [openai|claude]
_k=$(grep -E '^VOLCES_API_KEY_8230=' "$HOME/.hermes/.env" | cut -d= -f2-)
export CLAW_API_KEY="$_k"
export CLAW_MODEL="${CLAW_MODEL:-doubao-seed-evolving}"
if [ "${1:-openai}" = "claude" ]; then
  export CLAW_PROVIDER=claude
  export CLAW_BASE_URL="https://ark.cn-beijing.volces.com/api/coding"
else
  export CLAW_PROVIDER=openai
  export CLAW_BASE_URL="https://ark.cn-beijing.volces.com/api/coding/v3"
fi
unset _k
