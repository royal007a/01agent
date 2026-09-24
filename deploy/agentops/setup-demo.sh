#!/usr/bin/env bash
# 第 22 讲 AgentOps 演示工作区：AGENTS.md + 运维 Skill + 权限规则 + 故障现场（坏 nginx.conf + 巨型 error.log）
set -euo pipefail
dst="${1:?用法: setup-demo.sh <工作区目录>}"
src="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$dst"
cp -R "$src/AGENTS.md" "$src/.claw" "$dst/"
cat > "$dst/nginx.conf" <<'CONF'
server {
    listen 80;
    server_name localhost;
    # 这里故意写错一个指令，导致 Nginx 启动失败
    locat / {
        proxy_pass http://backend;
    }
}
CONF
yes '2026/04/24 23:58:00 [info] 12345#0: *123 client 192.168.1.1 connected' | head -n 2000 > "$dst/error.log" || true  # yes 被 head 截断会收到 SIGPIPE
cat >> "$dst/error.log" <<'LOG'
2026/04/24 23:58:01 [emerg] 12345#0: unknown directive "locat" in nginx.conf:5
2026/04/24 23:59:12 [emerg] 12345#0: unknown directive "locat" in nginx.conf:5
LOG
echo "demo workspace ready: $dst （把 $src/bin 加进 PATH 以使用假 nginx）"
