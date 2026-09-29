//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	clawapp "github.com/weberzhao/go-tiny-claw/internal/app"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
)

// 动态知识更新：会话 1 用户裁决"不要用 logrus、统一用 slog"，会话 2（新会话）写新代码时应遵守。
func TestKnowledge_CrossSession(t *testing.T) {
	realProvider(t)
	wd := workspace(t)
	sh(t, wd, "go mod init demo >/dev/null 2>&1")
	a, err := clawapp.Build(clawapp.Options{WorkDir: wd, Provider: provider.ConfigFromEnv(), StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a.Engine.MaxTurns = 20
	rec1 := newRec()
	if err := a.Engine.Chat(ctxTimeout(t), ctxpkg.NewSession("chat-A", wd), "这个项目的日志规范：统一使用 Go 标准库 log/slog，禁止引入 logrus 或 zap。请按这个规范写一个 logger.go，提供 NewLogger() 返回 *slog.Logger。", rec1); err != nil {
		t.Fatal(err)
	}
	t.Logf("会话1 调用轨迹:\n%s", rec1.callsJoined())
	prompt := knowledge.RenderPrompt(wd)
	t.Logf("沉淀的知识:\n%s", prompt)
	if !strings.Contains(rec1.callsJoined(), "record_knowledge") || prompt == "" {
		t.Fatal("会话 1 应在交付前记录知识并被外层归位")
	}
	rec2 := newRec()
	if err := a.Engine.Chat(ctxTimeout(t), ctxpkg.NewSession("chat-B", wd), "新写一个 server.go：启动一个 HTTP 服务监听 :8080，启动时打印一条日志。", rec2); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(wd, "server.go"))
	t.Logf("server.go:\n%s", src)
	if strings.Contains(string(src), "logrus") || strings.Contains(string(src), "go.uber.org/zap") {
		t.Fatal("会话 2 违背了会话 1 的裁决")
	}
	if !strings.Contains(string(src), "slog") {
		t.Fatal("会话 2 应沿用 slog 规范")
	}
}
