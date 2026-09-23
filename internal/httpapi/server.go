// Package httpapi 是本地部署用的 HTTP 入口：与飞书 Reporter 同构的另一个"展示层"，
// 让没有飞书应用凭据的机器也能以服务形式使用 go-tiny-claw（第 09 讲 I/O 解耦的直接收益）。
//
//	POST /v1/chat      {"session":"s1","prompt":"..."}         → 同步运行直到任务结束
//	GET  /v1/approvals?session=s1                              → 列出待审批的高危操作
//	POST /v1/approve   {"session":"s1","task_id":"ab12cd","approve":true}
//	GET  /v1/sessions/{id}                                     → 会话概要与历史
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/keylock"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// Event 是一次运行中发生的可观测事件。
type Event struct {
	Type    string    `json:"type"` // thinking | tool_call | tool_result | message | approval
	Tool    string    `json:"tool,omitempty"`
	Content string    `json:"content,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
	At      time.Time `json:"at"`
}

// collectReporter 把引擎事件收集起来随响应返回；同时实现 Approver，走 HTTP 审批。
type collectReporter struct {
	mu        sync.Mutex
	events    []Event
	session   string
	approvals *permission.ApprovalManager
}

func (r *collectReporter) add(e Event) {
	e.At = time.Now()
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *collectReporter) OnThinking(context.Context) { r.add(Event{Type: "thinking"}) }
func (r *collectReporter) OnToolCall(_ context.Context, tool, args string) {
	r.add(Event{Type: "tool_call", Tool: tool, Content: textutil.Truncate(args, 2000, "...")})
}
func (r *collectReporter) OnToolResult(_ context.Context, tool, result string, isErr bool) {
	r.add(Event{Type: "tool_result", Tool: tool, Content: result, IsError: isErr})
}
func (r *collectReporter) OnMessage(_ context.Context, c string) {
	r.add(Event{Type: "message", Content: c})
}

func (r *collectReporter) RequestApproval(ctx context.Context, req permission.ApprovalRequest) (bool, string) {
	id := permission.NewTaskID()
	summary := fmt.Sprintf("%s %s (规则 %s)", req.ToolName, textutil.Truncate(req.Args, 500, "..."), req.Rule)
	return r.approvals.WaitWithSummary(ctx, id, approvalScope(r.session), summary, func() {
		r.add(Event{Type: "approval", Tool: req.ToolName, Content: "等待审批 task_id=" + id + " : " + summary})
	})
}

// Server 本地 HTTP 服务。
type Server struct {
	Engine    *engine.AgentEngine
	Sessions  *ctxpkg.SessionManager
	WorkDir   string
	Approvals *permission.ApprovalManager
	// Token 非空时要求 Authorization: Bearer <Token>
	Token string

	locks *keylock.Table // session -> 串行锁（引用计数回收，不会被海量 ID 撑爆）
	once  sync.Once
}

func (s *Server) lockTable() *keylock.Table {
	s.once.Do(func() {
		if s.locks == nil {
			s.locks = keylock.New()
		}
	})
	return s.locks
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat", s.auth(s.chat))
	mux.HandleFunc("GET /v1/approvals", s.auth(s.listApprovals))
	mux.HandleFunc("POST /v1/approve", s.auth(s.approve))
	mux.HandleFunc("GET /v1/sessions/{id}", s.auth(s.session))
	mux.HandleFunc("POST /v1/sessions/{id}/reset", s.auth(s.reset))
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func sessionKey(id string) string { return "http_" + id }

// approvalScope 给 HTTP 会话的审批加命名空间，避免通过 HTTP 冒充飞书 chat_id 审批别的渠道的任务。
func approvalScope(session string) string { return "http:" + session }

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
		Prompt  string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 JSON: {\"session\":\"s1\",\"prompt\":\"...\"}"})
		return
	}
	if req.Session == "" {
		req.Session = "default"
	}
	unlock := s.lockTable().Lock(req.Session) // 同一会话串行，防止并发改同一工作区
	defer unlock()

	sess := s.Sessions.GetOrCreate(sessionKey(req.Session), s.WorkDir)
	rep := &collectReporter{session: req.Session, approvals: s.Approvals}
	start := time.Now()
	err := s.Engine.Chat(r.Context(), sess, req.Prompt, rep)
	p, c, cost := sess.UsageSnapshot()

	rep.mu.Lock()
	events := rep.events
	rep.mu.Unlock()
	reply := ""
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "message" {
			reply = events[i].Content
			break
		}
	}
	resp := map[string]any{
		"session":  req.Session,
		"reply":    reply,
		"events":   events,
		"duration": time.Since(start).Round(time.Millisecond).String(),
		"usage":    map[string]any{"prompt_tokens": p, "completion_tokens": c, "cost_cny": cost},
	}
	code := http.StatusOK
	if err != nil {
		resp["error"] = err.Error()
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, resp)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 ?session=<会话名>"})
		return
	}
	writeJSON(w, http.StatusOK, s.Approvals.List(approvalScope(session)))
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
		TaskID  string `json:"task_id"`
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TaskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 JSON: {\"session\":\"s1\",\"task_id\":\"...\",\"approve\":true}"})
		return
	}
	if req.Session == "" {
		req.Session = "default"
	}
	reason := req.Reason
	if reason == "" {
		reason = "人类管理员已批准操作"
		if !req.Approve {
			reason = "人类管理员通过 HTTP 拒绝了该操作，请换一种更安全的方式，或向用户说明原因。"
		}
	}
	if err := s.Approvals.Resolve(req.TaskID, approvalScope(req.Session), req.Approve, reason); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": req.TaskID, "approved": req.Approve})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	sess := s.Sessions.GetOrCreate(sessionKey(r.PathValue("id")), s.WorkDir)
	p, c, cost := sess.UsageSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": r.PathValue("id"), "messages": sess.Len(), "history": sess.History(),
		"usage": map[string]any{"prompt_tokens": p, "completion_tokens": c, "cost_cny": cost},
	})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	unlock, ok := s.lockTable().TryLock(id)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该会话有任务正在执行，稍后再重置"})
		return
	}
	defer unlock()
	s.Sessions.Reset(sessionKey(id))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "reset": true})
}
