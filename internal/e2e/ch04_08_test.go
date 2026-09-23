//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// weatherTool 第 04 讲的伪造天气工具。
type weatherTool struct{ calls int }

func (w *weatherTool) Name() string { return "get_weather" }
func (w *weatherTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "get_weather", Description: "获取指定城市的当前天气情况。", InputSchema: map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}}, "required": []string{"city"}}}
}
func (w *weatherTool) Execute(context.Context, []byte) (string, error) {
	w.calls++
	return "API 返回：今天是晴天，气温 25 度。", nil
}

// 第 03/04 讲：真实 Provider + 两阶段思考开关。
func TestL04_ProviderAndThinking(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		wt := &weatherTool{}
		reg := tools.NewRegistry()
		reg.Register(toolAdapter{wt})
		eng := engine.NewAgentEngine(realProvider(t), reg, thinking, false)
		rec := newRec()
		if err := eng.Chat(ctxTimeout(t), ctxpkg.NewSession("l04", t.TempDir()), "我想去北京跑步，帮我查查天气适合吗？", rec); err != nil {
			t.Fatal(err)
		}
		// 慢思考模式下模型可能想补充风力/AQI 而再查一次（实测 doubao 会），但不应无限重复
		t.Logf("thinking=%v: get_weather 调用 %d 次", thinking, wt.calls)
		if wt.calls < 1 || wt.calls > 3 || !(strings.Contains(rec.lastMsg(), "25") || strings.Contains(rec.lastMsg(), "晴")) {
			t.Fatalf("thinking=%v: 工具调用 %d 次，回复: %s", thinking, wt.calls, rec.lastMsg())
		}
	}
}

// toolAdapter 把 weatherTool 适配为 tools.BaseTool（Execute 签名使用 json.RawMessage）。
type toolAdapter struct{ w *weatherTool }

func (a toolAdapter) Name() string                      { return a.w.Name() }
func (a toolAdapter) Definition() schema.ToolDefinition { return a.w.Definition() }
func (a toolAdapter) Execute(ctx context.Context, b json.RawMessage) (string, error) {
	return a.w.Execute(ctx, b)
}

// 第 05 讲：read_file 第一次物理交互。
func TestL05_ReadFile(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "hello.txt"), "Hello, go-tiny-claw 引擎！我是来自物理文件系统的一段神秘文本，暗号是 MOONLIGHT-7。")
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	rec := newRec()
	if err := eng.Chat(ctxTimeout(t), ctxpkg.NewSession("l05", wd), "请调用工具读取一下当前工作区目录下 hello.txt 文件的内容，并用一句话向我总结它说了什么，并告诉我暗号。", rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.callsJoined(), "read_file") || !strings.Contains(rec.lastMsg(), "MOONLIGHT-7") {
		t.Fatalf("calls=%s reply=%s", rec.callsJoined(), rec.lastMsg())
	}
}

// 第 06 讲：bash + write_file 组合拳：查看 Go 版本、写 helloworld.go、编译运行。
func TestL06_BashWriteCombo(t *testing.T) {
	wd := workspace(t)
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	rec := newRec()
	prompt := "请帮我执行以下操作：\n1. 用 bash 查看一下我当前电脑的 Go 版本。\n2. 帮我写一个简单的 helloworld.go 文件，输出 \"Hello, go-tiny-claw!\"。\n3. 用 bash 编译并运行这个 go 文件，确认它能正常工作。"
	if err := eng.Chat(ctxTimeout(t), ctxpkg.NewSession("l06", wd), prompt, rec); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "run", filepath.Join(wd, "helloworld.go")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Hello, go-tiny-claw!") {
		t.Fatalf("helloworld.go 不可运行: %v %s", err, out)
	}
	if !strings.Contains(rec.callsJoined(), "go version") {
		t.Fatal("未查看 Go 版本")
	}
}

// 第 07 讲：带缩进的局部替换。
func TestL07_FuzzyEdit(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "server.go"), "package main\n\nimport \"fmt\"\n\nfunc main() {\n    // 启动服务器\n    fmt.Println(\"Server is starting on port 8080...\")\n    // TODO: 增加鉴权逻辑\n    if true {\n        fmt.Println(\"No auth, everyone can access.\")\n    }\n}\n")
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	prompt := "我当前目录下有一个 server.go 文件。\n请帮我把里面 \"TODO: 增加鉴权逻辑\" 下面的那个 if 语句，整个替换为：\nif user == nil {\nfmt.Println(\"Forbidden!\")\nreturn\n}"
	if err := eng.Chat(ctxTimeout(t), ctxpkg.NewSession("l07", wd), prompt, newRec()); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(wd, "server.go"))
	t.Logf("server.go:\n%s", src)
	if !strings.Contains(string(src), "if user == nil {") || strings.Contains(string(src), "if true") || !strings.Contains(string(src), "Server is starting") {
		t.Fatal("替换结果错误")
	}
}

// 第 08 讲：一次性并发读取三个文件。
func TestL08_ParallelReads(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "a.txt"), "这是文件A，里面记录了前端的报错日志。")
	write(t, filepath.Join(wd, "b.txt"), "这是文件B，里面记录了后端的接口响应时间。")
	write(t, filepath.Join(wd, "c.txt"), "这是文件C，里面写着今天的日期是周五。")
	lb := captureLog(t)
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), true, false)
	rec := newRec()
	if err := eng.Chat(ctxTimeout(t), ctxpkg.NewSession("l08", wd), "我当前目录下有 a.txt, b.txt, c.txt 三个文件。为了节省时间，请你在同一次回复中同时发起三个 read_file 调用一次性读取这三个文件，并将它们的内容综合起来，告诉我它们分别记录了什么领域的信息。", rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lb.String(), "本批 3 个工具均为只读，Fork 并发执行") {
		t.Fatalf("未并发读取:\n%s", rec.callsJoined())
	}
}
