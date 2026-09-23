package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

type rmProvider struct{}

func (rmProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	last := msgs[len(msgs)-1]
	if last.IsToolResult() {
		return &schema.Message{Role: schema.RoleAssistant, Content: "结果: " + last.Content, Usage: &schema.Usage{PromptTokens: 5, CompletionTokens: 1}}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c", Name: "bash", Arguments: []byte(`{"command":"rm -rf build && echo cleaned"}`)}}}, nil
}

func post(t *testing.T, url, token string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestChatWithHTTPApproval(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, "build"), 0o755)
	reg := tools.NewRegistry()
	reg.Register(tools.NewBashTool(wd))
	reg.Use(permission.Middleware(permission.NewPolicy("")))
	s := &Server{Engine: engine.NewAgentEngine(observability.NewCostTracker(rmProvider{}, "m", nil), reg, false, false), Sessions: ctxpkg.NewSessionManager(""),
		WorkDir: wd, Approvals: permission.NewApprovalManager(5 * time.Second), Token: "tk"}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code, _ := post(t, srv.URL+"/v1/chat", "bad", map[string]string{"prompt": "x"}); code != 401 {
		t.Fatal("token 校验失效")
	}
	done := make(chan map[string]any)
	go func() {
		_, out := post(t, srv.URL+"/v1/chat", "tk", map[string]string{"session": "s1", "prompt": "清理 build"})
		done <- out
	}()
	var taskID string
	for i := 0; i < 200 && taskID == ""; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/approvals?session=s1", nil)
		req.Header.Set("Authorization", "Bearer tk")
		resp, _ := http.DefaultClient.Do(req)
		var list []permission.PendingInfo
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if len(list) == 1 {
			taskID = list[0].TaskID
			if !strings.Contains(list[0].Summary, "rm -rf build") {
				t.Fatalf("摘要: %s", list[0].Summary)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := post(t, srv.URL+"/v1/approve", "tk", map[string]any{"session": "other", "task_id": taskID, "approve": true}); code != 409 {
		t.Fatal("其他会话不应能审批")
	}
	if code, _ := post(t, srv.URL+"/v1/approve", "tk", map[string]any{"session": "s1", "task_id": taskID, "approve": true}); code != 200 {
		t.Fatal("审批失败")
	}
	out := <-done
	if !strings.Contains(out["reply"].(string), "cleaned") {
		t.Fatalf("reply: %v", out)
	}
	if _, err := os.Stat(filepath.Join(wd, "build")); !os.IsNotExist(err) {
		t.Fatal("审批后应真正执行")
	}
	if u := out["usage"].(map[string]any); u["prompt_tokens"].(float64) != 5 {
		t.Fatalf("usage: %v", u)
	}
}
