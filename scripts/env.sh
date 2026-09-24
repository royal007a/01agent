# 本机真实模型配置：从 ~/.hermes/.env 读取方舟（Volcengine Ark）Coding Plan key，不落盘复制。
# 用法: source scripts/env.sh [openai|claude|ollama]
#   ollama：本机 Ollama（OpenAI 兼容 localhost:11434/v1），模型默认 qwen2.5:7b，可用 CLAW_MODEL 覆盖
_k=$(grep -E '^VOLCES_API_KEY_8230=' "$HOME/.hermes/.env" | cut -d= -f2-)
export CLAW_API_KEY="$_k"
export CLAW_MODEL="${CLAW_MODEL:-doubao-seed-evolving}"
if [ "${1:-openai}" = "ollama" ]; then
  export CLAW_PROVIDER=openai
  export CLAW_API_KEY=ollama
  export CLAW_BASE_URL="http://localhost:11434/v1"
  [ "$CLAW_MODEL" = "doubao-seed-evolving" ] && export CLAW_MODEL=qwen2.5:7b
  export CLAW_MODEL="${CLAW_MODEL:-qwen2.5:7b}"
  export CLAW_PRICE_IN=0 CLAW_PRICE_OUT=0
elif [ "${1:-openai}" = "claude" ]; then
  export CLAW_PROVIDER=claude
  export CLAW_BASE_URL="https://ark.cn-beijing.volces.com/api/coding"
else
  export CLAW_PROVIDER=openai
  export CLAW_BASE_URL="https://ark.cn-beijing.volces.com/api/coding/v3"
fi
unset _k
