# 接口与功能验证矩阵

最后基线：`3a93dd8`。离线验证命令：

```bash
go vet ./...
go test -race ./...
```

## 自动化覆盖

| 范围 | 证据 | 覆盖内容 |
|---|---|---|
| Provider | `internal/provider/provider_test.go` | OpenAI/Claude 请求翻译、tool blocks、Usage、model 回传 |
| Main Loop | `internal/engine/loop_test.go`、`reminder_test.go` | ReAct、并发/串行工具、Reminder/熔断、MaxTurns |
| 文件与 Bash | `internal/tools/tools_test.go` | 路径边界、UTF-8 截断、原子写、超时、后台进程、只读白名单 |
| 权限与审批 | `internal/permission/permission_test.go`、`internal/httpapi/server_test.go`、`internal/feishu/*_test.go` | allow/ask/deny、同会话 scope、超时/取消、HTTP/飞书审批 |
| Session/Context | `internal/context/*_test.go` | 隔离、持久化恢复、损坏尾行修复、Compaction、Prompt/Skill |
| Retrieval | `internal/retrieval/retrieval_test.go`、`internal/tools/retrieval_test.go` | 动态 Chunk、父子聚合、账本事务、Evidence 门禁、预算/停止、RRF、指标 |
| Knowledge | `internal/knowledge/knowledge_test.go`、`internal/engine/knowledge_test.go` | Inbox 校验/隔离、journal 重放、去重/取代、严格 4KB 注入、每次唤醒一次清点 |
| Environment lock | `internal/envlock/envlock_test.go`、`internal/app/app_test.go`、`internal/eval/envdrift_test.go` | 快照、Diff 分级、strict/warn、模型哨兵、注入 LLM 字段豁免 |
| Trace/Evaluator | `internal/trace/*_test.go`、`internal/eval/benchmark_test.go` | Span 树、Perfetto 导出、判卷、空 validate 拒绝、环境指纹 |
| App assembly | `internal/app/app_test.go`、`internal/httpapi/agentops_test.go` | Provider、权限、检索、知识、子 Agent 只读守卫的统一装配 |

## 接口级手工验证

```bash
make test
curl -fsS http://127.0.0.1:48080/healthz
curl -fsS http://127.0.0.1:48080/metrics
curl -sS -X POST http://127.0.0.1:48080/v1/chat \
  -H 'Content-Type: application/json' \
  -d '{"session":"spec-smoke","prompt":"只读取当前 Go 版本并回复"}'
```

启用 `CLAW_HTTP_TOKEN` 时，为每个请求补 `Authorization: Bearer <token>`，并验证无 Token 返回 401。审批接口应验证：缺 session 返回 400、跨 session approve 返回 409、同 session approve/reject 后工具结果分别为成功/错误。

## 真实模型与部署边界

- `go test -tags live ./internal/e2e -v -run L19`、`L22` 和 `make bench` 需要可用模型额度，不能用离线绿灯替代。
- 当前部署默认是本机 launchd `127.0.0.1:48080`；公网反代、飞书凭据和 132 服务器属于单独部署环境，必须以实际 `/healthz`、`/metrics` 和 API 请求结果为证。
- Benchmark 的 Setup/Validate 是可信输入下的宿主机 Bash，不等价于 OS 沙箱。
