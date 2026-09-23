# go-tiny-claw

跟随极客时间《从 0 开始构建 Agent Harness》第 02–18 讲，用 Go 从零实现的 Agent Harness：
两阶段 ReAct Main Loop、双协议 Provider、极简工具集（read/write/edit/bash）、并发工具执行、
飞书接入、AGENTS.md/Skills 提示词组装、Session 与 Working Memory、Context Compaction、
Plan Mode、Error Recovery、System Reminders、权限中间件与人工审批、Subagent、成本追踪。

- 实现总结与课程差异：[docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)
- 真实模型验证记录：[docs/VERIFICATION.md](docs/VERIFICATION.md)

```bash
source scripts/env.sh openai          # 配置真实模型（或自行设置 CLAW_PROVIDER/CLAW_API_KEY/CLAW_BASE_URL/CLAW_MODEL）
go run ./cmd/claw -workdir /tmp/ws -prompt "用 bash 看一下 go 版本，再写个 hello.go 跑起来"
make test                             # 离线测试
make live RUN=L15                     # 真实模型复现第 15 讲
make deploy                           # 本地 launchd 常驻服务 http://127.0.0.1:48080
```
